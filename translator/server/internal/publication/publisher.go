package publication

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
)

const (
	TranslatorKindProp           = "translator_kind"
	TranslatorOperationIDProp    = "translator_operation_id"
	TranslatorSourcePostIDProp   = "translator_source_post_id"
	TranslatorSourceUpdateAtProp = "translator_source_update_at"
	TranslatorModelProp          = "translator_model"
)

var (
	ErrReconcileUncertain    = errors.New("publication reconciliation is uncertain")
	ErrOperationPostNotFound = errors.New("publication operation post was not found")
	ErrStaleKnownPost        = errors.New("known automatic post has a newer source version")
	ErrPublicationGuardBusy  = errors.New("automatic publication guard is busy")
)

type PublicationAPI interface {
	CreatePost(post *model.Post) (*model.Post, *model.AppError)
	UpdatePost(post *model.Post) (*model.Post, *model.AppError)
	GetPost(postID string) (*model.Post, *model.AppError)
	GetPostsForChannel(channelID string, page, perPage int) (*model.PostList, *model.AppError)
	DeletePost(postID string) *model.AppError
}

type DeletionIntentRepository interface {
	PutDeletionIntent(intent domain.DeletionIntent, expireInSeconds int64) error
}

type AutomaticPublicationGuardRepository interface {
	AcquireAutomaticPublicationGuard(
		desired domain.AutomaticPublicationGuard,
		now time.Time,
	) (domain.AutomaticPublicationGuard, bool, error)
	BeginAutomaticPublicationUpdate(
		before domain.AutomaticPublicationGuard,
	) (domain.AutomaticPublicationGuard, bool, error)
	ReleaseAutomaticPublicationGuard(before domain.AutomaticPublicationGuard) (bool, error)
}

type Operation struct {
	ID        string
	StartedAt time.Time
	ChannelID string
	UserID    string
}

type Request struct {
	OperationID         string
	OperationStartedAt  time.Time
	ChannelID           string
	RootID              string
	UserID              string
	Message             string
	Props               model.StringInterface
	KnownPostID         string
	PublicationGuard    domain.AutomaticPublicationGuard
	PublicationGuardNow time.Time
}

type Result struct {
	PostID string
	Post   *model.Post
}

type Publisher struct {
	api               PublicationAPI
	deletionIntents   DeletionIntentRepository
	publicationGuards AutomaticPublicationGuardRepository
}

func NewPublisher(
	api PublicationAPI,
	deletionIntents DeletionIntentRepository,
	publicationGuards ...AutomaticPublicationGuardRepository,
) *Publisher {
	publisher := &Publisher{api: api, deletionIntents: deletionIntents}
	if len(publicationGuards) > 0 {
		publisher.publicationGuards = publicationGuards[0]
	}
	return publisher
}

func (p *Publisher) Publish(ctx context.Context, request Request) (Result, error) {
	operation := Operation{
		ID:        request.OperationID,
		StartedAt: request.OperationStartedAt,
		ChannelID: request.ChannelID,
		UserID:    request.UserID,
	}
	if err := validateOperation(operation); err != nil {
		return Result{}, err
	}
	if err := contextError(ctx); err != nil {
		return Result{}, err
	}

	if request.KnownPostID != "" {
		result, continueWithScan, err := p.publishKnown(ctx, request)
		if err != nil || !continueWithScan {
			return result, err
		}
	}

	matches, err := p.scan(ctx, operation)
	if err != nil {
		return Result{}, err
	}
	if len(matches) > 0 {
		return p.adoptMatches(ctx, operation, matches)
	}

	post := &model.Post{
		UserId:        request.UserID,
		ChannelId:     request.ChannelID,
		RootId:        request.RootID,
		Message:       request.Message,
		PendingPostId: request.OperationID,
	}
	post.SetProps(copyProps(request.Props))
	post.AddProp(TranslatorOperationIDProp, request.OperationID)

	if err := contextError(ctx); err != nil {
		return Result{}, err
	}
	created, appErr := p.api.CreatePost(post)
	if appErr == nil && created != nil && created.Id != "" {
		return resultForPost(created), nil
	}

	matches, scanErr := p.scan(ctx, operation)
	if scanErr != nil {
		return Result{}, scanErr
	}
	if len(matches) > 0 {
		return p.adoptMatches(ctx, operation, matches)
	}
	if appErr != nil {
		return Result{}, uncertain("create acknowledgement could not be reconciled", appErr)
	}
	return Result{}, uncertain("create returned no post", nil)
}

func (p *Publisher) publishKnown(ctx context.Context, request Request) (result Result, continueWithScan bool, err error) {
	if !validAutomaticRequestIdentity(request) {
		return Result{}, false, uncertain("known automatic request identity is incomplete", nil)
	}
	if err := contextError(ctx); err != nil {
		return Result{}, false, err
	}
	known, appErr := p.api.GetPost(request.KnownPostID)
	if appErr != nil {
		if appErr.StatusCode == http.StatusNotFound {
			return Result{}, true, nil
		}
		return Result{}, false, uncertain("known post lookup failed", appErr)
	}
	if known == nil || known.DeleteAt != 0 {
		return Result{}, true, nil
	}

	if !matchesAutomaticIdentity(known, request) {
		return Result{}, false, uncertain("known post identity does not match the automatic translation", nil)
	}
	knownVersion, knownVersionOK := automaticSourceVersion(known.GetProp(TranslatorSourceUpdateAtProp))
	requestedVersion, requestedVersionOK := automaticSourceVersion(request.Props[TranslatorSourceUpdateAtProp])
	if knownVersionOK && requestedVersionOK && knownVersion > requestedVersion {
		return Result{}, false, ErrStaleKnownPost
	}
	if hasDesiredVersion(known, request) {
		return resultForPost(known), false, nil
	}
	if p.publicationGuards == nil || !validAutomaticPublicationGuard(request, requestedVersion, requestedVersionOK) {
		return Result{}, false, uncertain("automatic known-post update guard is incomplete", nil)
	}

	var heldGuard *domain.AutomaticPublicationGuard
	defer func() {
		if heldGuard == nil {
			return
		}
		released, releaseErr := p.publicationGuards.ReleaseAutomaticPublicationGuard(*heldGuard)
		if releaseErr != nil {
			err = errors.Join(err, uncertain("automatic publication guard release failed", releaseErr))
			return
		}
		if !released {
			err = errors.Join(err, uncertain("automatic publication guard release was not confirmed", nil))
		}
	}()
	if request.PublicationGuard.TranslationPostID != "" {
		acquired, ok, acquireErr := p.publicationGuards.AcquireAutomaticPublicationGuard(
			request.PublicationGuard,
			request.PublicationGuardNow,
		)
		if acquireErr != nil {
			return Result{}, false, uncertain("automatic publication guard acquisition failed", acquireErr)
		}
		if !ok {
			if acquired.SourceUpdateAt > request.PublicationGuard.SourceUpdateAt {
				return Result{}, false, ErrStaleKnownPost
			}
			return Result{}, false, ErrPublicationGuardBusy
		}
		heldGuard = &acquired

		known, appErr = p.api.GetPost(request.KnownPostID)
		if appErr != nil {
			if appErr.StatusCode == http.StatusNotFound {
				return Result{}, true, nil
			}
			return Result{}, false, uncertain("guarded known post reload failed", appErr)
		}
		if known == nil || known.DeleteAt != 0 {
			return Result{}, true, nil
		}
		if !matchesAutomaticIdentity(known, request) {
			return Result{}, false, uncertain("guarded known post identity changed", nil)
		}
		knownVersion, knownVersionOK = automaticSourceVersion(known.GetProp(TranslatorSourceUpdateAtProp))
		if knownVersionOK && requestedVersionOK && knownVersion > requestedVersion {
			return Result{}, false, ErrStaleKnownPost
		}
		if hasDesiredVersion(known, request) {
			return resultForPost(known), false, nil
		}
		updating, ok, beginErr := p.publicationGuards.BeginAutomaticPublicationUpdate(acquired)
		if beginErr != nil {
			return Result{}, false, uncertain("automatic publication update fence failed", beginErr)
		}
		if !ok {
			heldGuard = nil
			if updating.SourceUpdateAt > acquired.SourceUpdateAt {
				return Result{}, false, ErrStaleKnownPost
			}
			return Result{}, false, ErrPublicationGuardBusy
		}
		heldGuard = &updating
	}

	updated := known.Clone()
	updated.Message = request.Message
	props := copyProps(updated.GetProps())
	for key, value := range request.Props {
		props[key] = value
	}
	updated.SetProps(props)
	updated.AddProp(TranslatorOperationIDProp, request.OperationID)

	if err := contextError(ctx); err != nil {
		return Result{}, false, err
	}
	acknowledged, updateErr := p.api.UpdatePost(updated)
	if updateErr == nil && acknowledged != nil && acknowledged.Id == known.Id {
		return resultForPost(acknowledged), false, nil
	}

	adopted, reloadErr := p.reloadDesiredKnown(ctx, request)
	if reloadErr != nil {
		return Result{}, false, reloadErr
	}
	if adopted != nil {
		return resultForPost(adopted), false, nil
	}
	return Result{}, false, uncertain("update acknowledgement could not be reconciled", updateErr)
}

func validAutomaticPublicationGuard(request Request, requestedVersion int64, requestedVersionOK bool) bool {
	guard := request.PublicationGuard
	return requestedVersionOK && !request.PublicationGuardNow.IsZero() &&
		guard.TranslationPostID == request.KnownPostID &&
		guard.SourcePostID == propString(request.Props[TranslatorSourcePostIDProp]) &&
		guard.SourceUpdateAt == requestedVersion &&
		guard.OperationID == request.OperationID &&
		guard.LeaseUntil.After(request.PublicationGuardNow)
}

func validAutomaticRequestIdentity(request Request) bool {
	return propEqual(request.Props[TranslatorKindProp], "automatic") &&
		propString(request.Props[TranslatorSourcePostIDProp]) != "" &&
		request.Props[TranslatorSourceUpdateAtProp] != nil
}

func (p *Publisher) reloadDesiredKnown(ctx context.Context, request Request) (*model.Post, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	post, appErr := p.api.GetPost(request.KnownPostID)
	if appErr != nil {
		return nil, uncertain("known post reload failed", appErr)
	}
	if post == nil || post.DeleteAt != 0 || !matchesAutomaticIdentity(post, request) || !hasDesiredVersion(post, request) {
		return nil, nil
	}
	return post, nil
}

func matchesAutomaticIdentity(post *model.Post, request Request) bool {
	if post == nil || post.UserId != request.UserID || post.ChannelId != request.ChannelID {
		return false
	}
	if !propEqual(post.GetProp(TranslatorKindProp), "automatic") {
		return false
	}
	return propEqual(post.GetProp(TranslatorSourcePostIDProp), request.Props[TranslatorSourcePostIDProp])
}

func hasDesiredVersion(post *model.Post, request Request) bool {
	return propEqual(post.GetProp(TranslatorOperationIDProp), request.OperationID) &&
		propEqual(post.GetProp(TranslatorSourceUpdateAtProp), request.Props[TranslatorSourceUpdateAtProp])
}

func propEqual(left, right any) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return fmt.Sprint(left) == fmt.Sprint(right)
}

func automaticSourceVersion(value any) (int64, bool) {
	if value == nil {
		return 0, false
	}
	version, err := strconv.ParseInt(fmt.Sprint(value), 10, 64)
	return version, err == nil
}

func copyProps(props model.StringInterface) model.StringInterface {
	copied := make(model.StringInterface, len(props)+1)
	for key, value := range props {
		copied[key] = value
	}
	return copied
}

func resultForPost(post *model.Post) Result {
	if post == nil {
		return Result{}
	}
	cloned := post.Clone()
	return Result{PostID: cloned.Id, Post: cloned}
}

func validateOperation(operation Operation) error {
	if operation.ID == "" || operation.ChannelID == "" || operation.UserID == "" {
		return uncertain("operation identity is incomplete", nil)
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func uncertain(message string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrReconcileUncertain, message)
	}
	return fmt.Errorf("%w: %s: %v", ErrReconcileUncertain, message, cause)
}
