package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
)

const (
	modelValidationTimeout = 2 * time.Second
	requestTimeout         = 10 * time.Second
	maximumGlossaryEntries = 200
	maximumGlossaryRunes   = 100
)

type configurationLoader interface {
	LoadPluginConfiguration(destination any) error
}

type ConfigurationAPI interface {
	GetTeamByName(name string) (*model.Team, *model.AppError)
	GetUserByUsername(username string) (*model.User, *model.AppError)
	GetUser(userID string) (*model.User, *model.AppError)
}

type ModelValidator interface {
	ValidateModels(ctx context.Context, apiKey, primary, fallback string) error
}

type systemConsoleConfiguration struct {
	GeminiAPIKey                    string
	PrimaryModel                    string
	FallbackModel                   string
	AutomaticTeam                   string
	AutomaticSourceUsers            string
	IncludeDirectMessages           bool
	SharedGlossary                  string
	MaximumConcurrentGeminiRequests int
	TotalRequestTimeoutSeconds      int
}

// runtimeConfiguration is replaced as a whole and never mutated after it is
// published. Its collections are newly allocated while validating settings.
type runtimeConfiguration struct {
	geminiAPIKey                    string
	primaryModel                    string
	fallbackModel                   string
	automaticTeamID                 string
	automaticSourceUserIDs          []string
	translatorBotUserID             string
	includeDirectMessages           bool
	glossary                        map[string]string
	maximumConcurrentGeminiRequests int
	totalRequestTimeout             time.Duration
	siteURL                         string
	activationAt                    int64
}

func defaultSystemConsoleConfiguration() systemConsoleConfiguration {
	return systemConsoleConfiguration{
		PrimaryModel:                    "gemini-3.7-flash",
		FallbackModel:                   "gemini-3.6-flash",
		IncludeDirectMessages:           true,
		MaximumConcurrentGeminiRequests: 8,
		TotalRequestTimeoutSeconds:      10,
	}
}

func (p *Plugin) loadAndValidateConfiguration(ctx context.Context) (_ *runtimeConfiguration, err error) {
	generation := p.beginConfigurationLoad()
	defer func() {
		if err != nil {
			p.replaceConfigurationIfCurrent(generation, nil)
		}
	}()

	loader := p.configurationLoader
	if loader == nil {
		loader = p.API
	}
	if loader == nil {
		return nil, errors.New("configuration could not be loaded")
	}

	configured := defaultSystemConsoleConfiguration()
	if loadErr := loader.LoadPluginConfiguration(&configured); loadErr != nil {
		return nil, errors.New("configuration could not be loaded")
	}

	apiKey := configured.GeminiAPIKey
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("GeminiAPIKey must not be empty")
	}
	primaryModel := strings.TrimSpace(configured.PrimaryModel)
	if primaryModel == "" {
		return nil, errors.New("PrimaryModel must not be empty")
	}
	fallbackModel := strings.TrimSpace(configured.FallbackModel)
	if fallbackModel == "" {
		return nil, errors.New("FallbackModel must not be empty")
	}
	teamName := strings.TrimSpace(configured.AutomaticTeam)
	if teamName == "" {
		return nil, errors.New("AutomaticTeam must not be empty")
	}
	if configured.MaximumConcurrentGeminiRequests < 1 || configured.MaximumConcurrentGeminiRequests > 64 {
		return nil, errors.New("MaximumConcurrentGeminiRequests must be between 1 and 64")
	}
	if configured.TotalRequestTimeoutSeconds != int(requestTimeout/time.Second) {
		return nil, errors.New("TotalRequestTimeoutSeconds must be 10")
	}

	glossary, glossaryErr := parseGlossary(configured.SharedGlossary)
	if glossaryErr != nil {
		return nil, glossaryErr
	}
	sourceUsernames, sourceErr := normalizeSourceUsernames(configured.AutomaticSourceUsers)
	if sourceErr != nil {
		return nil, sourceErr
	}

	configurationAPI := p.configurationAPI
	if configurationAPI == nil {
		configurationAPI = p.API
	}
	if configurationAPI == nil {
		return nil, errors.New("configuration identity API is unavailable")
	}

	team, appErr := configurationAPI.GetTeamByName(teamName)
	if appErr != nil || team == nil || team.Id == "" || team.DeleteAt != 0 {
		return nil, errors.New("AutomaticTeam does not identify an active team")
	}

	sourceUserIDs := make([]string, 0, len(sourceUsernames))
	seenSourceUserIDs := make(map[string]struct{}, len(sourceUsernames))
	for _, username := range sourceUsernames {
		user, userErr := configurationAPI.GetUserByUsername(username)
		if userErr != nil || user == nil || user.Id == "" || user.DeleteAt != 0 {
			return nil, errors.New("AutomaticSourceUsers contains a user that is missing or inactive")
		}
		if _, exists := seenSourceUserIDs[user.Id]; exists {
			return nil, errors.New("AutomaticSourceUsers resolves to a duplicate user")
		}
		seenSourceUserIDs[user.Id] = struct{}{}
		sourceUserIDs = append(sourceUserIDs, user.Id)
	}

	translatorByName, translatorErr := configurationAPI.GetUserByUsername("translator")
	if translatorErr != nil || translatorByName == nil || translatorByName.Id == "" {
		return nil, errors.New("translator bot account is missing")
	}
	translator, translatorErr := configurationAPI.GetUser(translatorByName.Id)
	if translatorErr != nil || translator == nil || translator.Id != translatorByName.Id {
		return nil, errors.New("translator bot account could not be resolved")
	}
	if translator.DeleteAt != 0 {
		return nil, errors.New("translator bot account is inactive")
	}
	if !translator.IsBot {
		return nil, errors.New("translator account must be a bot")
	}
	roles := strings.Fields(translator.Roles)
	if len(roles) != 1 || roles[0] != "system_user" {
		return nil, errors.New("translator bot roles must be exactly system_user")
	}

	if p.modelValidator == nil {
		return nil, errors.New("PrimaryModel/FallbackModel validation is unavailable")
	}
	validationContext, cancel := context.WithTimeout(ctx, modelValidationTimeout)
	defer cancel()
	if validationErr := p.modelValidator.ValidateModels(validationContext, apiKey, primaryModel, fallbackModel); validationErr != nil {
		return nil, errors.New("PrimaryModel/FallbackModel validation failed")
	}

	validated := &runtimeConfiguration{
		geminiAPIKey:                    apiKey,
		primaryModel:                    primaryModel,
		fallbackModel:                   fallbackModel,
		automaticTeamID:                 team.Id,
		automaticSourceUserIDs:          sourceUserIDs,
		translatorBotUserID:             translator.Id,
		includeDirectMessages:           configured.IncludeDirectMessages,
		glossary:                        glossary,
		maximumConcurrentGeminiRequests: configured.MaximumConcurrentGeminiRequests,
		totalRequestTimeout:             requestTimeout,
	}
	if !p.replaceConfigurationIfCurrent(generation, validated) {
		return nil, errors.New("configuration change was superseded by a newer change")
	}
	return validated, nil
}

func (p *Plugin) getConfiguration() (*runtimeConfiguration, bool) {
	p.configurationMu.RLock()
	defer p.configurationMu.RUnlock()
	if p.configuration == nil {
		return nil, false
	}
	return cloneRuntimeConfiguration(p.configuration), true
}

func (p *Plugin) beginConfigurationLoad() uint64 {
	p.configurationMu.Lock()
	defer p.configurationMu.Unlock()
	p.configurationGeneration++
	return p.configurationGeneration
}

func (p *Plugin) replaceConfigurationIfCurrent(generation uint64, configuration *runtimeConfiguration) bool {
	p.configurationMu.Lock()
	defer p.configurationMu.Unlock()
	if generation != p.configurationGeneration {
		return false
	}
	p.configuration = cloneRuntimeConfiguration(configuration)
	return true
}

func cloneRuntimeConfiguration(configuration *runtimeConfiguration) *runtimeConfiguration {
	if configuration == nil {
		return nil
	}
	cloned := *configuration
	cloned.automaticSourceUserIDs = append([]string(nil), configuration.automaticSourceUserIDs...)
	cloned.glossary = make(map[string]string, len(configuration.glossary))
	for korean, english := range configuration.glossary {
		cloned.glossary[korean] = english
	}
	return &cloned
}

func normalizeSourceUsernames(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	usernames := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		username := strings.TrimSpace(part)
		username = strings.TrimPrefix(username, "@")
		username = strings.ToLower(username)
		if username == "" {
			return nil, errors.New("AutomaticSourceUsers must contain only non-empty usernames")
		}
		if _, exists := seen[username]; exists {
			return nil, errors.New("AutomaticSourceUsers contains a duplicate username")
		}
		seen[username] = struct{}{}
		usernames = append(usernames, username)
	}
	return usernames, nil
}

func parseGlossary(raw string) (map[string]string, error) {
	glossary := make(map[string]string)
	for lineNumber, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.Count(line, "=") != 1 {
			return nil, fmt.Errorf("SharedGlossary line %d must use Korean = English form", lineNumber+1)
		}
		parts := strings.SplitN(line, "=", 2)
		korean := strings.TrimSpace(parts[0])
		english := strings.TrimSpace(parts[1])
		if korean == "" || english == "" {
			return nil, fmt.Errorf("SharedGlossary line %d must have non-empty Korean and English values", lineNumber+1)
		}
		if utf8.RuneCountInString(korean) > maximumGlossaryRunes || utf8.RuneCountInString(english) > maximumGlossaryRunes {
			return nil, fmt.Errorf("SharedGlossary line %d values must not exceed 100 Unicode code points", lineNumber+1)
		}
		if _, exists := glossary[korean]; exists {
			return nil, errors.New("SharedGlossary contains a duplicate Korean key")
		}
		if len(glossary) == maximumGlossaryEntries {
			return nil, errors.New("SharedGlossary must not contain more than 200 entries")
		}
		glossary[korean] = english
	}
	return glossary, nil
}
