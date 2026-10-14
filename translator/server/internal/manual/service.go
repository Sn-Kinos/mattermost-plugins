package manual

import (
	"context"
	"errors"
	"sync"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/gemini"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/limit"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
)

type API interface {
	GetPost(postID string) (*model.Post, *model.AppError)
	CreatePost(post *model.Post) (*model.Post, *model.AppError)
	UpdatePost(post *model.Post) (*model.Post, *model.AppError)
	SendEphemeralPost(userID string, post *model.Post) *model.Post
}

type Translator interface {
	TranslateToEnglish(ctx context.Context, input gemini.EnglishInput, glossary map[string]string) (string, error)
}

type ServiceConfig struct {
	TranslatorUserID string
	Glossary         map[string]string
}

// translatingPlaceholder heads the caller's post until the translation lands.
// It is English because /trans readers are the English-speaking audience.
const translatingPlaceholder = "Translating…"

// errNotPosted marks failures before the caller's post exists, where the
// cleared command input would otherwise lose the message.
var errNotPosted = errors.New("manual message was not posted")

// Service posts /trans messages immediately as a placeholder head over the
// quoted Korean source, then edits the head into the English translation.
type Service struct {
	api              API
	translator       Translator
	gate             *limit.Gate
	translatorUserID string
	glossary         map[string]string

	lifecycleContext context.Context
	cancelLifecycle  context.CancelFunc
	lifecycleMu      sync.Mutex
	stopping         bool
	background       sync.WaitGroup
}

func NewService(parent context.Context, api API, translator Translator, gate *limit.Gate, config ServiceConfig) *Service {
	if parent == nil {
		parent = context.Background()
	}
	lifecycleContext, cancelLifecycle := context.WithCancel(parent)
	glossary := make(map[string]string, len(config.Glossary))
	for korean, english := range config.Glossary {
		glossary[korean] = english
	}
	return &Service{
		api:              api,
		translator:       translator,
		gate:             gate,
		translatorUserID: config.TranslatorUserID,
		glossary:         glossary,
		lifecycleContext: lifecycleContext,
		cancelLifecycle:  cancelLifecycle,
	}
}

// Execute posts the /trans message with fileIDs attached. Mattermost attaches
// only the caller's own unattached uploads, so fileIDs need no checks here.
func (s *Service) Execute(_ context.Context, args *model.CommandArgs, fileIDs []string) (*model.CommandResponse, error) {
	if args == nil || s == nil || s.api == nil || s.translator == nil || s.gate == nil ||
		args.UserId == "" || args.ChannelId == "" {
		return privateCommandError(), nil
	}
	message, err := Parse(args.Command)
	if err != nil {
		return privateCommandError(), nil
	}
	release, acquired := s.gate.TryAcquire()
	if !acquired {
		return privateResponse("현재 번역 요청이 많습니다. 잠시 후 다시 시도해 주세요."), nil
	}
	work := *args
	if !s.launch(func() {
		defer release()
		if err := s.translateAndPost(&work, message, fileIDs); err != nil {
			s.sendPrivateFailure(&work, message, errors.Is(err, errNotPosted))
		}
	}) {
		release()
		return privateCommandError(), nil
	}
	return nil, nil
}

func (s *Service) translateAndPost(args *model.CommandArgs, message string, fileIDs []string) error {
	parentContext := ""
	if args.RootId != "" {
		root, appErr := s.api.GetPost(args.RootId)
		if appErr != nil || root == nil || root.DeleteAt != 0 || root.ChannelId != args.ChannelId {
			return errNotPosted
		}
		parentContext = root.Message
	}
	placeholder, _ := publication.ApplyInlineTranslation(&model.Post{
		UserId: args.UserId, ChannelId: args.ChannelId, RootId: args.RootId, Message: message, FileIds: fileIDs,
	}, translatingPlaceholder)
	posted, appErr := s.api.CreatePost(placeholder)
	if appErr != nil || posted == nil {
		return errNotPosted
	}

	translation, err := s.translator.TranslateToEnglish(
		s.lifecycleContext,
		gemini.EnglishInput{Message: message, ParentContext: parentContext},
		s.glossary,
	)
	current, appErr := s.api.GetPost(posted.Id)
	if appErr != nil || current == nil || current.DeleteAt != 0 ||
		!publication.HasInlineTranslation(current, translatingPlaceholder) {
		// The caller edited or deleted the post meanwhile; their version wins.
		return nil
	}
	if err != nil {
		s.restoreSource(current, message)
		return err
	}
	updated, ok := publication.ApplyInlineTranslation(current, translation)
	if !ok {
		s.restoreSource(current, message)
		return errors.New("manual translation is empty")
	}
	if _, appErr := s.api.UpdatePost(updated); appErr != nil {
		s.restoreSource(current, message)
		return errors.New("manual post could not be updated")
	}
	return nil
}

// restoreSource drops the placeholder so a failed translation leaves the
// caller's plain Korean message.
func (s *Service) restoreSource(post *model.Post, message string) {
	restored := post.Clone()
	restored.Message = message
	restored.DelProp(publication.InlineBlockHashProp)
	restored.DelProp(publication.InlineLayoutProp)
	_, _ = s.api.UpdatePost(restored)
}

func (s *Service) sendPrivateFailure(args *model.CommandArgs, message string, notPosted bool) {
	text := "번역을 완료하지 못해 원문만 게시했습니다. 잠시 후 다시 시도해 주세요."
	if notPosted {
		text = "메시지를 게시하지 못했습니다. 잠시 후 다시 시도해 주세요.\n\n```\n" + message + "\n```"
	}
	s.api.SendEphemeralPost(args.UserId, &model.Post{
		Id:        model.NewId(),
		UserId:    s.translatorUserID,
		ChannelId: args.ChannelId,
		RootId:    args.RootId,
		Message:   text,
	})
}

func (s *Service) launch(work func()) bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.stopping || s.lifecycleContext.Err() != nil {
		return false
	}
	s.background.Add(1)
	go func() {
		defer s.background.Done()
		work()
	}()
	return true
}

func (s *Service) Shutdown() {
	if s == nil {
		return
	}
	s.lifecycleMu.Lock()
	if !s.stopping {
		s.stopping = true
		s.cancelLifecycle()
	}
	s.lifecycleMu.Unlock()
	s.background.Wait()
}

func privateCommandError() *model.CommandResponse {
	return privateResponse("번역을 완료하지 못했습니다. 잠시 후 다시 시도해 주세요.")
}

func privateResponse(text string) *model.CommandResponse {
	return &model.CommandResponse{ResponseType: model.CommandResponseTypeEphemeral, Text: text}
}
