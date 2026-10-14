package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAPIKey = "top-secret-api-key"

type configurationLoaderStub struct {
	configuration systemConsoleConfiguration
	err           error
}

func (s configurationLoaderStub) LoadPluginConfiguration(destination any) error {
	if s.err != nil {
		return s.err
	}
	configuration, ok := destination.(*systemConsoleConfiguration)
	if !ok {
		return errors.New("unexpected configuration destination")
	}
	*configuration = s.configuration
	return nil
}

type configurationAPIStub struct {
	teams           map[string]*model.Team
	usersByUsername map[string]*model.User
	usersByID       map[string]*model.User
}

func (s *configurationAPIStub) GetTeamByName(name string) (*model.Team, *model.AppError) {
	return s.teams[name], nil
}

func (s *configurationAPIStub) GetUserByUsername(username string) (*model.User, *model.AppError) {
	return s.usersByUsername[username], nil
}

func (s *configurationAPIStub) GetUser(userID string) (*model.User, *model.AppError) {
	return s.usersByID[userID], nil
}

type modelValidatorStub struct {
	err         error
	apiKey      string
	primary     string
	fallback    string
	hasDeadline bool
	remaining   time.Duration
}

type sequencedConfigurationLoader struct {
	mu             sync.Mutex
	configurations []systemConsoleConfiguration
	callCount      int
	secondLoaded   chan struct{}
}

func (s *sequencedConfigurationLoader) LoadPluginConfiguration(destination any) error {
	configuration, ok := destination.(*systemConsoleConfiguration)
	if !ok {
		return errors.New("unexpected configuration destination")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.callCount >= len(s.configurations) {
		return errors.New("unexpected configuration load")
	}
	*configuration = s.configurations[s.callCount]
	s.callCount++
	if s.callCount == 2 {
		close(s.secondLoaded)
	}
	return nil
}

type blockingModelValidator struct {
	entered chan struct{}
	release chan struct{}
}

func (v *blockingModelValidator) ValidateModels(context.Context, string, string, string) error {
	close(v.entered)
	<-v.release
	return nil
}

func (s *modelValidatorStub) ValidateModels(ctx context.Context, apiKey, primary, fallback string) error {
	s.apiKey = apiKey
	s.primary = primary
	s.fallback = fallback
	deadline, ok := ctx.Deadline()
	s.hasDeadline = ok
	if ok {
		s.remaining = time.Until(deadline)
	}
	return s.err
}

func validSystemConsoleConfiguration() systemConsoleConfiguration {
	return systemConsoleConfiguration{
		GeminiAPIKey:                    testAPIKey,
		PrimaryModel:                    "gemini-3.7-flash",
		FallbackModel:                   "gemini-3.6-flash",
		AutomaticTeam:                   "example",
		AutomaticSourceUsers:            "alex",
		IncludeDirectMessages:           true,
		MaximumConcurrentGeminiRequests: 8,
		TotalRequestTimeoutSeconds:      10,
	}
}

func validConfigurationAPI() *configurationAPIStub {
	alex := &model.User{Id: "user-alex", Username: "alex", DeleteAt: 0}
	translator := &model.User{
		Id:       "user-translator",
		Username: "translator",
		DeleteAt: 0,
		IsBot:    true,
		Roles:    "system_user",
	}
	return &configurationAPIStub{
		teams: map[string]*model.Team{
			"example": {Id: "team-example", Name: "example", DeleteAt: 0},
		},
		usersByUsername: map[string]*model.User{
			"alex":       alex,
			"translator": translator,
		},
		usersByID: map[string]*model.User{
			"user-translator": translator,
		},
	}
}

func newConfigurationTestPlugin(configuration systemConsoleConfiguration, api *configurationAPIStub, validator *modelValidatorStub) *Plugin {
	return &Plugin{
		configurationLoader: configurationLoaderStub{configuration: configuration},
		configurationAPI:    api,
		modelValidator:      validator,
	}
}

func TestConfigurationDefaultsRequireTeamAndSourceUsers(t *testing.T) {
	defaults := defaultSystemConsoleConfiguration()
	assert.Empty(t, defaults.AutomaticTeam)
	assert.Empty(t, defaults.AutomaticSourceUsers)

	configuration := defaults
	configuration.GeminiAPIKey = testAPIKey
	plugin := newConfigurationTestPlugin(configuration, validConfigurationAPI(), &modelValidatorStub{})
	_, err := plugin.loadAndValidateConfiguration(context.Background())
	require.EqualError(t, err, "AutomaticTeam must not be empty")
}

func TestConfigurationResolvesTeamSourceUsersAndTranslator(t *testing.T) {
	configuration := validSystemConsoleConfiguration()
	configuration.AutomaticTeam = "  example  "
	configuration.AutomaticSourceUsers = " @ALEX "
	configuration.PrimaryModel = "  gemini-3.7-flash "
	configuration.FallbackModel = " gemini-3.6-flash  "
	validator := &modelValidatorStub{}
	plugin := newConfigurationTestPlugin(configuration, validConfigurationAPI(), validator)

	loaded, err := plugin.loadAndValidateConfiguration(context.Background())
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "team-example", loaded.automaticTeamID)
	assert.Equal(t, []string{"user-alex"}, loaded.automaticSourceUserIDs)
	assert.Equal(t, "user-translator", loaded.translatorBotUserID)
	assert.Equal(t, "gemini-3.7-flash", loaded.primaryModel)
	assert.Equal(t, "gemini-3.6-flash", loaded.fallbackModel)
	assert.Equal(t, 8, loaded.maximumConcurrentGeminiRequests)
	assert.Equal(t, 10*time.Second, loaded.totalRequestTimeout)
	assert.True(t, loaded.includeDirectMessages)
	assert.Empty(t, loaded.glossary)

	assert.Equal(t, testAPIKey, validator.apiKey)
	assert.Equal(t, "gemini-3.7-flash", validator.primary)
	assert.Equal(t, "gemini-3.6-flash", validator.fallback)
	assert.True(t, validator.hasDeadline)
	assert.Greater(t, validator.remaining, 1500*time.Millisecond)
	assert.LessOrEqual(t, validator.remaining, 2*time.Second)

	active, ok := plugin.getConfiguration()
	assert.True(t, ok)
	assert.Equal(t, loaded, active)
	assert.NotSame(t, loaded, active)
}

func TestConfigurationRejectsDuplicateInactiveAndMissingSourceUsers(t *testing.T) {
	tests := []struct {
		name        string
		sourceUsers string
		mutateAPI   func(*configurationAPIStub)
	}{
		{
			name:        "duplicate after normalization",
			sourceUsers: "alex, @ALEX",
		},
		{
			name:        "inactive",
			sourceUsers: "alex",
			mutateAPI: func(api *configurationAPIStub) {
				api.usersByUsername["alex"].DeleteAt = 42
			},
		},
		{
			name:        "missing",
			sourceUsers: "missing",
		},
		{
			name:        "empty entry",
			sourceUsers: "alex,  ",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configuration := validSystemConsoleConfiguration()
			configuration.AutomaticSourceUsers = test.sourceUsers
			api := validConfigurationAPI()
			if test.mutateAPI != nil {
				test.mutateAPI(api)
			}
			plugin := newConfigurationTestPlugin(configuration, api, &modelValidatorStub{})

			_, err := plugin.loadAndValidateConfiguration(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "AutomaticSourceUsers")
			_, ok := plugin.getConfiguration()
			assert.False(t, ok)
		})
	}
}

func TestConfigurationRejectsMissingInactiveNonBotAdminAndPostAllTranslator(t *testing.T) {
	tests := []struct {
		name      string
		mutateAPI func(*configurationAPIStub)
	}{
		{
			name: "missing username",
			mutateAPI: func(api *configurationAPIStub) {
				delete(api.usersByUsername, "translator")
			},
		},
		{
			name: "missing resolved ID",
			mutateAPI: func(api *configurationAPIStub) {
				delete(api.usersByID, "user-translator")
			},
		},
		{
			name: "inactive",
			mutateAPI: func(api *configurationAPIStub) {
				api.usersByID["user-translator"].DeleteAt = 42
			},
		},
		{
			name: "not a bot",
			mutateAPI: func(api *configurationAPIStub) {
				api.usersByID["user-translator"].IsBot = false
			},
		},
		{
			name: "system administrator",
			mutateAPI: func(api *configurationAPIStub) {
				api.usersByID["user-translator"].Roles = "system_user system_admin"
			},
		},
		{
			name: "post all",
			mutateAPI: func(api *configurationAPIStub) {
				api.usersByID["user-translator"].Roles = "system_post_all system_user"
			},
		},
		{
			name: "post all public",
			mutateAPI: func(api *configurationAPIStub) {
				api.usersByID["user-translator"].Roles = "system_user system_post_all_public"
			},
		},
		{
			name: "non ordinary role",
			mutateAPI: func(api *configurationAPIStub) {
				api.usersByID["user-translator"].Roles = "system_user custom_role"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := validConfigurationAPI()
			test.mutateAPI(api)
			plugin := newConfigurationTestPlugin(validSystemConsoleConfiguration(), api, &modelValidatorStub{})

			_, err := plugin.loadAndValidateConfiguration(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "translator")
			assert.NotContains(t, err.Error(), testAPIKey)
		})
	}
}

func TestConfigurationRejectsInvalidTeamModelsKeyConcurrencyAndTimeout(t *testing.T) {
	tests := []struct {
		name           string
		wantField      string
		mutateConfig   func(*systemConsoleConfiguration)
		mutateAPI      func(*configurationAPIStub)
		validatorError error
	}{
		{
			name:      "empty API key",
			wantField: "GeminiAPIKey",
			mutateConfig: func(configuration *systemConsoleConfiguration) {
				configuration.GeminiAPIKey = " \t "
			},
		},
		{
			name:      "missing team",
			wantField: "AutomaticTeam",
			mutateConfig: func(configuration *systemConsoleConfiguration) {
				configuration.AutomaticTeam = "missing"
			},
		},
		{
			name:      "inactive team",
			wantField: "AutomaticTeam",
			mutateAPI: func(api *configurationAPIStub) {
				api.teams["example"].DeleteAt = 42
			},
		},
		{
			name:      "empty primary model",
			wantField: "PrimaryModel",
			mutateConfig: func(configuration *systemConsoleConfiguration) {
				configuration.PrimaryModel = "  "
			},
		},
		{
			name:      "empty fallback model",
			wantField: "FallbackModel",
			mutateConfig: func(configuration *systemConsoleConfiguration) {
				configuration.FallbackModel = "  "
			},
		},
		{
			name:           "unavailable model",
			wantField:      "PrimaryModel/FallbackModel",
			validatorError: fmt.Errorf("provider echoed %s", testAPIKey),
		},
		{
			name:      "zero concurrency",
			wantField: "MaximumConcurrentGeminiRequests",
			mutateConfig: func(configuration *systemConsoleConfiguration) {
				configuration.MaximumConcurrentGeminiRequests = 0
			},
		},
		{
			name:      "concurrency above limit",
			wantField: "MaximumConcurrentGeminiRequests",
			mutateConfig: func(configuration *systemConsoleConfiguration) {
				configuration.MaximumConcurrentGeminiRequests = 65
			},
		},
		{
			name:      "timeout below release value",
			wantField: "TotalRequestTimeoutSeconds",
			mutateConfig: func(configuration *systemConsoleConfiguration) {
				configuration.TotalRequestTimeoutSeconds = 9
			},
		},
		{
			name:      "timeout above release value",
			wantField: "TotalRequestTimeoutSeconds",
			mutateConfig: func(configuration *systemConsoleConfiguration) {
				configuration.TotalRequestTimeoutSeconds = 11
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configuration := validSystemConsoleConfiguration()
			if test.mutateConfig != nil {
				test.mutateConfig(&configuration)
			}
			api := validConfigurationAPI()
			if test.mutateAPI != nil {
				test.mutateAPI(api)
			}
			plugin := newConfigurationTestPlugin(configuration, api, &modelValidatorStub{err: test.validatorError})

			_, err := plugin.loadAndValidateConfiguration(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantField)
			assert.NotContains(t, err.Error(), testAPIKey)
			_, ok := plugin.getConfiguration()
			assert.False(t, ok)
		})
	}
}

func TestConfigurationParsesManualGlossaryWithoutLoggingSecrets(t *testing.T) {
	configuration := validSystemConsoleConfiguration()
	configuration.SharedGlossary = " 배포 = deployment \n\n회의 = meeting\r\n"
	plugin := newConfigurationTestPlugin(configuration, validConfigurationAPI(), &modelValidatorStub{})

	loaded, err := plugin.loadAndValidateConfiguration(context.Background())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"배포": "deployment",
		"회의": "meeting",
	}, loaded.glossary)

	invalidGlossaries := []struct {
		name     string
		glossary string
	}{
		{name: "missing separator", glossary: "배포 deployment"},
		{name: "multiple separators", glossary: "배포 = deploy = release"},
		{name: "empty Korean", glossary: " = deployment"},
		{name: "empty English", glossary: "배포 = "},
		{name: "duplicate Korean", glossary: "배포 = deployment\n배포 = release"},
		{name: "Korean over 100 code points", glossary: strings.Repeat("가", 101) + " = word"},
		{name: "English over 100 code points", glossary: "용어 = " + strings.Repeat("x", 101)},
		{name: "more than 200 entries", glossary: glossaryEntries(201)},
	}

	for _, test := range invalidGlossaries {
		t.Run(test.name, func(t *testing.T) {
			configuration := validSystemConsoleConfiguration()
			configuration.SharedGlossary = test.glossary
			plugin := newConfigurationTestPlugin(configuration, validConfigurationAPI(), &modelValidatorStub{})

			_, err := plugin.loadAndValidateConfiguration(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "SharedGlossary")
			assert.NotContains(t, err.Error(), testAPIKey)
		})
	}

	configuration = validSystemConsoleConfiguration()
	configuration.SharedGlossary = "배포 = deployment"
	plugin = newConfigurationTestPlugin(configuration, validConfigurationAPI(), &modelValidatorStub{})
	_, err = plugin.loadAndValidateConfiguration(context.Background())
	require.NoError(t, err)

	plugin.configurationLoader = configurationLoaderStub{err: fmt.Errorf("failed with %s", testAPIKey)}
	_, err = plugin.loadAndValidateConfiguration(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "configuration")
	assert.NotContains(t, err.Error(), testAPIKey)
	_, ok := plugin.getConfiguration()
	assert.False(t, ok, "a failed settings change must clear the prior active configuration")
}

func TestConfigurationErrorsNeverExposeAPIKeyWhenConfiguredValuesContainIt(t *testing.T) {
	tests := []struct {
		name      string
		wantField string
		mutate    func(*systemConsoleConfiguration)
	}{
		{
			name:      "team name",
			wantField: "AutomaticTeam",
			mutate: func(configuration *systemConsoleConfiguration) {
				configuration.AutomaticTeam = testAPIKey
			},
		},
		{
			name:      "source username",
			wantField: "AutomaticSourceUsers",
			mutate: func(configuration *systemConsoleConfiguration) {
				configuration.AutomaticSourceUsers = testAPIKey
			},
		},
		{
			name:      "duplicate glossary key",
			wantField: "SharedGlossary",
			mutate: func(configuration *systemConsoleConfiguration) {
				configuration.SharedGlossary = testAPIKey + " = first\n" + testAPIKey + " = second"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configuration := validSystemConsoleConfiguration()
			test.mutate(&configuration)
			plugin := newConfigurationTestPlugin(configuration, validConfigurationAPI(), &modelValidatorStub{})

			_, err := plugin.loadAndValidateConfiguration(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantField)
			assert.NotContains(t, err.Error(), testAPIKey)
		})
	}
}

func TestConfigurationOlderSuccessCannotOverrideNewerInvalidChange(t *testing.T) {
	valid := validSystemConsoleConfiguration()
	invalid := validSystemConsoleConfiguration()
	invalid.AutomaticTeam = "missing"
	loader := &sequencedConfigurationLoader{
		configurations: []systemConsoleConfiguration{valid, invalid},
		secondLoaded:   make(chan struct{}),
	}
	validator := &blockingModelValidator{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	plugin := &Plugin{
		configurationLoader: loader,
		configurationAPI:    validConfigurationAPI(),
		modelValidator:      validator,
	}

	olderDone := make(chan error, 1)
	go func() {
		_, err := plugin.loadAndValidateConfiguration(context.Background())
		olderDone <- err
	}()
	<-validator.entered

	newerDone := make(chan error, 1)
	go func() {
		_, err := plugin.loadAndValidateConfiguration(context.Background())
		newerDone <- err
	}()
	<-loader.secondLoaded
	require.Error(t, <-newerDone)

	close(validator.release)
	require.Error(t, <-olderDone, "a superseded reload must not return a usable stale snapshot")
	_, ok := plugin.getConfiguration()
	assert.False(t, ok, "an older successful reload must not republish after a newer change failed")
}

func TestConfigurationPublishedSnapshotIsImmuneToReturnedValueMutation(t *testing.T) {
	configuration := validSystemConsoleConfiguration()
	configuration.SharedGlossary = "배포 = deployment"
	plugin := newConfigurationTestPlugin(configuration, validConfigurationAPI(), &modelValidatorStub{})

	loaded, err := plugin.loadAndValidateConfiguration(context.Background())
	require.NoError(t, err)
	loaded.automaticSourceUserIDs[0] = "mutated-load-result"
	loaded.glossary["배포"] = "mutated-load-result"

	firstRead, ok := plugin.getConfiguration()
	require.True(t, ok)
	assert.Equal(t, []string{"user-alex"}, firstRead.automaticSourceUserIDs)
	assert.Equal(t, map[string]string{"배포": "deployment"}, firstRead.glossary)
	firstRead.automaticSourceUserIDs[0] = "mutated-read-result"
	firstRead.glossary["배포"] = "mutated-read-result"

	secondRead, ok := plugin.getConfiguration()
	require.True(t, ok)
	assert.NotSame(t, firstRead, secondRead)
	assert.Equal(t, []string{"user-alex"}, secondRead.automaticSourceUserIDs)
	assert.Equal(t, map[string]string{"배포": "deployment"}, secondRead.glossary)
}

func glossaryEntries(count int) string {
	lines := make([]string, count)
	for index := range lines {
		lines[index] = fmt.Sprintf("용어%d = word%d", index, index)
	}
	return strings.Join(lines, "\n")
}
