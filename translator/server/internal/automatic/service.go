package automatic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/gemini"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/store"
)

type Repository interface {
	PutJob(job domain.AutomaticJob) error
	GetJob(sourcePostID string, sourceUpdateAt int64) (domain.AutomaticJob, bool, error)
	CreateMapping(mapping domain.SourceMapping) (bool, error)
	SaveMappingCAS(beforeToken string, after domain.SourceMapping) (bool, error)
	GetMapping(sourcePostID string) (domain.SourceMapping, bool, error)
	GetMappingForUpdate(sourcePostID string) (domain.SourceMapping, string, bool, error)
	ListMappings() ([]domain.SourceMapping, error)
	PutDeletionIntent(intent domain.DeletionIntent, expireInSeconds int64) error
	GetDeletionIntent(translationPostID string) (domain.DeletionIntent, bool, error)
	ConsumeDeletionIntent(translationPostID string) (bool, error)
	CreateAutomaticPublicationOutcome(outcome domain.AutomaticPublicationOutcome) (bool, error)
	SaveAutomaticPublicationOutcomeCAS(
		before, after domain.AutomaticPublicationOutcome,
	) (bool, error)
	GetAutomaticPublicationOutcome(operationID string) (domain.AutomaticPublicationOutcome, bool, error)
	ListAutomaticPublicationOutcomes() ([]domain.AutomaticPublicationOutcome, error)
	DeleteAutomaticPublicationOutcomeCAS(outcome domain.AutomaticPublicationOutcome) (bool, error)
	GetAutomaticPublicationGuard(translationPostID string) (domain.AutomaticPublicationGuard, bool, error)
	ListAutomaticPublicationGuards() ([]domain.AutomaticPublicationGuard, error)
	AcquireAutomaticPublicationGuard(
		desired domain.AutomaticPublicationGuard,
		now time.Time,
	) (domain.AutomaticPublicationGuard, bool, error)
	BeginAutomaticPublicationUpdate(before domain.AutomaticPublicationGuard) (domain.AutomaticPublicationGuard, bool, error)
	ReleaseAutomaticPublicationGuard(before domain.AutomaticPublicationGuard) (bool, error)
	SaveAutomaticPublicationGuardCAS(before, after domain.AutomaticPublicationGuard) (bool, error)
}

const (
	maximumMappingTransitionTries  = 8
	automaticPublicationGuardLease = 5 * time.Minute
)

var ErrMappingContention = errors.New("automatic mapping contention")

type LifecycleAPI interface {
	GetPost(postID string) (*model.Post, *model.AppError)
	UpdatePost(post *model.Post) (*model.Post, *model.AppError)
	DeletePost(postID string) *model.AppError
}

type KoreanTranslator interface {
	TranslateToKorean(ctx context.Context, input gemini.KoreanInput, glossary map[string]string) (domain.KoreanTranslation, error)
}

type PostPublisher interface {
	Publish(ctx context.Context, request publication.Request) (publication.Result, error)
	Reconcile(ctx context.Context, operation publication.Operation) (publication.Result, error)
}

// Waker is implemented by the durable worker scheduler. Wake must be
// non-blocking; handlers call it only after every required record is durable.
type Waker interface {
	Wake()
}

type ServiceConfig struct {
	ActivationAt     int64
	TranslatorUserID string
	SiteURL          string
	ModelID          string
	Glossary         map[string]string
	Now              func() time.Time
}

type Service struct {
	scope            *Scope
	repository       Repository
	api              LifecycleAPI
	translator       KoreanTranslator
	publisher        PostPublisher
	waker            Waker
	activationAt     int64
	translatorUserID string
	siteURL          string
	modelID          string
	glossary         map[string]string
	now              func() time.Time
	newIntentID      func() string
}

func NewService(
	scope *Scope,
	repository *store.Repository,
	api publication.PublicationAPI,
	translator KoreanTranslator,
	waker Waker,
	config ServiceConfig,
) *Service {
	return newService(
		scope,
		repository,
		api,
		translator,
		publication.NewPublisher(api, repository, repository),
		waker,
		config,
		model.NewId,
	)
}

// SetActivationAt binds a runtime assembled before activation to the one
// cutoff selected for the active plugin lifecycle. Plugin lifecycle locking
// calls this before hooks and workers can use the service.
func (s *Service) SetActivationAt(activationAt int64) {
	if s != nil {
		s.activationAt = activationAt
	}
}

func newService(
	scope *Scope,
	repository Repository,
	api LifecycleAPI,
	translator KoreanTranslator,
	publisher PostPublisher,
	waker Waker,
	config ServiceConfig,
	newIntentID func() string,
) *Service {
	now := config.Now
	if now == nil {
		now = time.Now
	}
	if newIntentID == nil {
		newIntentID = model.NewId
	}
	return &Service{
		scope:            scope,
		repository:       repository,
		api:              api,
		translator:       translator,
		publisher:        publisher,
		waker:            waker,
		activationAt:     config.ActivationAt,
		translatorUserID: config.TranslatorUserID,
		siteURL:          config.SiteURL,
		modelID:          config.ModelID,
		glossary:         copyGlossary(config.Glossary),
		now:              now,
		newIntentID:      newIntentID,
	}
}

func (s *Service) ProcessJob(ctx context.Context, job domain.AutomaticJob) error {
	if s == nil || s.scope == nil || s.repository == nil || s.api == nil || s.translator == nil || s.publisher == nil {
		return errors.New("automatic service is unavailable")
	}
	resolved, err := s.resolveRecordedPublication(ctx, job)
	if err != nil || resolved {
		return err
	}
	source, appErr := s.api.GetPost(job.SourcePostID)
	if isMissingPost(source, appErr) {
		return s.cancelSourceMapping(job.SourcePostID)
	}
	if appErr != nil {
		return errors.New("automatic source reload failed")
	}
	if sourceVersion(source) != job.SourceUpdateAt {
		return nil
	}

	mapping, found, err := s.repository.GetMapping(job.SourcePostID)
	if err != nil {
		return fmt.Errorf("load automatic mapping: %w", err)
	}
	if !found || !mappingMatchesJob(mapping, job) || mappingCancelled(mapping) {
		return nil
	}
	eligible, err := s.scope.Eligible(source, 0)
	if err != nil {
		return err
	}
	if !eligible {
		return nil
	}

	var root *model.Post
	if source.RootId != "" {
		root, appErr = s.api.GetPost(source.RootId)
		if isMissingPost(root, appErr) {
			return errors.New("automatic thread root is unavailable")
		}
		if appErr != nil {
			return errors.New("automatic thread root reload failed")
		}
	}
	translationSource := source.Clone()
	translationSource.Message, _ = publication.StripInlineTranslation(source)
	if root != nil {
		root = root.Clone()
		root.Message, _ = publication.StripInlineTranslation(root)
	}
	input, ok, err := BuildTranslationInput(translationSource, root)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	translation, err := s.translator.TranslateToKorean(ctx, input, copyGlossary(s.glossary))
	if err != nil {
		return err
	}
	restored, err := RestoreProtectedContent(translation.Text, input.Message)
	if err != nil {
		return err
	}
	currentSource, appErr := s.api.GetPost(job.SourcePostID)
	if isMissingPost(currentSource, appErr) {
		return s.cancelSourceMapping(job.SourcePostID)
	}
	if appErr != nil {
		return errors.New("automatic source revalidation failed")
	}
	if sourceVersion(currentSource) != job.SourceUpdateAt {
		return nil
	}
	currentMapping, found, err := s.repository.GetMapping(job.SourcePostID)
	if err != nil {
		return fmt.Errorf("revalidate automatic mapping: %w", err)
	}
	if !found || !mappingMatchesJob(currentMapping, job) || mappingCancelled(currentMapping) {
		return nil
	}
	eligible, err = s.scope.Eligible(currentSource, 0)
	if err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	return s.updateSourceInline(job, currentMapping, currentSource, renderMentionsAsInlineCode(restored))
}

func (s *Service) updateSourceInline(
	job domain.AutomaticJob,
	mapping domain.SourceMapping,
	source *model.Post,
	translation string,
) error {
	updated, ok := publication.ApplyInlineTranslation(source, translation)
	if !ok {
		return errors.New("automatic inline translation is empty")
	}
	acknowledged, updateErr := s.api.UpdatePost(updated)
	if updateErr == nil && automaticInlineUpdateMatches(acknowledged, source, translation) {
		return s.finalizeInlineMapping(job, mapping, acknowledged)
	}
	reloaded, reloadErr := s.api.GetPost(source.Id)
	if reloadErr != nil || !automaticInlineUpdateMatches(reloaded, source, translation) {
		return errors.New("automatic inline update could not be confirmed")
	}
	return s.finalizeInlineMapping(job, mapping, reloaded)
}

func automaticInlineUpdateMatches(updated, source *model.Post, translation string) bool {
	return updated != nil && source != nil && updated.DeleteAt == 0 &&
		updated.Id == source.Id && updated.UserId == source.UserId && updated.ChannelId == source.ChannelId &&
		publication.HasInlineTranslation(updated, translation)
}

func (s *Service) finalizeInlineMapping(
	job domain.AutomaticJob,
	before domain.SourceMapping,
	updated *model.Post,
) error {
	for range maximumMappingTransitionTries {
		current, token, found, err := s.repository.GetMappingForUpdate(job.SourcePostID)
		if err != nil {
			return fmt.Errorf("reload automatic inline mapping: %w", err)
		}
		if !found || !mappingMatchesJob(current, job) || mappingCancelled(current) {
			return nil
		}
		after := current
		after.SourceUpdateAt = sourceVersion(updated)
		after.OperationID = automaticOperationID(after.SourcePostID, after.SourceUpdateAt)
		after.OperationStartedAt = s.now().UTC()
		after.TranslationPostID = after.SourcePostID
		after.SuppressedUpdateAt = 0
		after.InternallyDeleting = false
		stored, err := s.repository.SaveMappingCAS(token, after)
		if err != nil {
			return fmt.Errorf("save automatic inline mapping: %w", err)
		}
		if !stored {
			continue
		}
		if before.TranslationPostID != "" && before.TranslationPostID != before.SourcePostID {
			return s.deleteLegacyAutomaticTranslation(before)
		}
		return nil
	}
	return ErrMappingContention
}

func (s *Service) deleteLegacyAutomaticTranslation(mapping domain.SourceMapping) error {
	post, appErr := s.api.GetPost(mapping.TranslationPostID)
	if isMissingPost(post, appErr) {
		return nil
	}
	if appErr != nil {
		return errors.New("automatic legacy translation lookup failed")
	}
	if post.UserId != s.translatorUserID || post.ChannelId != mapping.ChannelID ||
		!automaticPropertyEqual(post.GetProp(publication.TranslatorKindProp), "automatic") ||
		!automaticPropertyEqual(post.GetProp(publication.TranslatorSourcePostIDProp), mapping.SourcePostID) {
		return nil
	}
	operationID := fmt.Sprint(post.GetProp(publication.TranslatorOperationIDProp))
	if operationID == "" {
		operationID = mapping.OperationID
	}
	return s.deletePostWithIntent(post.Id, mapping.SourcePostID, operationID)
}

func (s *Service) resolveRecordedPublication(ctx context.Context, job domain.AutomaticJob) (bool, error) {
	outcome, found, err := s.repository.GetAutomaticPublicationOutcome(job.OperationID)
	if err != nil {
		return false, fmt.Errorf("load automatic publication outcome: %w", err)
	}
	if !found {
		return false, nil
	}
	return s.resolvePublicationOutcome(ctx, job, outcome)
}

func (s *Service) resolvePublicationOutcome(
	ctx context.Context,
	job domain.AutomaticJob,
	outcome domain.AutomaticPublicationOutcome,
) (bool, error) {
	if outcome.State == domain.AutomaticPublicationPublishing && outcome.TranslationPostID == "" {
		result, safeRetry, err := s.reconcileEmptyPublicationIntent(ctx, outcome)
		if err != nil {
			return true, err
		}
		if safeRetry {
			deleted, deleteErr := s.repository.DeleteAutomaticPublicationOutcomeCAS(outcome)
			if deleteErr != nil {
				return true, fmt.Errorf("delete empty automatic publication intent: %w", deleteErr)
			}
			if !deleted {
				return true, ErrMappingContention
			}
			return false, nil
		}
		if result.PostID == "" {
			return true, errors.New("automatic publication intent reconciliation returned no post")
		}
		published := outcome
		published.State = domain.AutomaticPublicationPublished
		published.TranslationPostID = result.PostID
		stored, err := s.repository.SaveAutomaticPublicationOutcomeCAS(outcome, published)
		if err != nil {
			return true, fmt.Errorf("record reconciled automatic publication result: %w", err)
		}
		if !stored {
			return true, ErrMappingContention
		}
		outcome = published
	}
	if outcome.State != domain.AutomaticPublicationPublished || outcome.TranslationPostID == "" {
		return true, errors.New("automatic publication outcome is invalid")
	}
	if err := s.releaseOutcomePublicationGuard(outcome); err != nil {
		return true, err
	}
	source := &model.Post{Id: outcome.SourcePostID, ChannelId: outcome.ChannelID}
	if err := s.finalizePublishedPost(job, source, outcome.TranslationPostID); err != nil {
		return true, err
	}
	deleted, err := s.repository.DeleteAutomaticPublicationOutcomeCAS(outcome)
	if err != nil {
		return true, fmt.Errorf("delete resolved automatic publication outcome: %w", err)
	}
	if !deleted {
		return true, ErrMappingContention
	}
	return true, nil
}

type knownAutomaticPostState uint8

const (
	knownAutomaticPostMissing knownAutomaticPostState = iota
	knownAutomaticPostOld
	knownAutomaticPostCommitted
)

func (s *Service) reconcileEmptyPublicationIntent(
	ctx context.Context,
	outcome domain.AutomaticPublicationOutcome,
) (publication.Result, bool, error) {
	if err := ctx.Err(); err != nil {
		return publication.Result{}, false, err
	}
	if outcome.KnownPostID == "" {
		result, err := s.publisher.Reconcile(ctx, publication.Operation{
			ID:        outcome.OperationID,
			StartedAt: outcome.OperationStartedAt,
			ChannelID: outcome.ChannelID,
			UserID:    s.translatorUserID,
		})
		if errors.Is(err, publication.ErrOperationPostNotFound) {
			return publication.Result{}, true, nil
		}
		return result, false, err
	}

	post, state, err := s.inspectKnownAutomaticPost(
		outcome.KnownPostID,
		outcome.SourcePostID,
		outcome.ChannelID,
		outcome.OperationID,
		outcome.SourceUpdateAt,
	)
	if err != nil {
		return publication.Result{}, false, err
	}
	switch state {
	case knownAutomaticPostCommitted:
		return publication.Result{PostID: post.Id, Post: post}, false, nil
	case knownAutomaticPostOld:
		return publication.Result{}, true, nil
	case knownAutomaticPostMissing:
		intent, found, intentErr := s.repository.GetDeletionIntent(outcome.KnownPostID)
		if intentErr != nil {
			return publication.Result{}, false, fmt.Errorf("load automatic known-post deletion intent: %w", intentErr)
		}
		if found && intent.TranslationPostID == outcome.KnownPostID &&
			intent.SourcePostID == outcome.SourcePostID && intent.OperationID != "" {
			return publication.Result{}, true, nil
		}
	default:
	}
	return publication.Result{}, false, errors.New("automatic known-post publication outcome is unavailable")
}

func (s *Service) inspectKnownAutomaticPost(
	postID, sourcePostID, channelID, operationID string,
	sourceUpdateAt int64,
) (*model.Post, knownAutomaticPostState, error) {
	post, appErr := s.api.GetPost(postID)
	if isMissingPost(post, appErr) {
		return nil, knownAutomaticPostMissing, nil
	}
	if appErr != nil {
		return nil, knownAutomaticPostMissing, errors.New("automatic known-post publication lookup failed")
	}
	if post.UserId != s.translatorUserID || post.ChannelId != channelID ||
		!automaticPropertyEqual(post.GetProp(publication.TranslatorKindProp), "automatic") ||
		!automaticPropertyEqual(post.GetProp(publication.TranslatorSourcePostIDProp), sourcePostID) {
		return nil, knownAutomaticPostMissing, errors.New("automatic known-post publication identity changed")
	}
	version, versionOK := automaticPostSourceVersion(post)
	postOperation := fmt.Sprint(post.GetProp(publication.TranslatorOperationIDProp))
	if !versionOK || postOperation == "" {
		return nil, knownAutomaticPostMissing, errors.New("automatic known-post publication metadata is incomplete")
	}
	if version == sourceUpdateAt && postOperation == operationID {
		return post, knownAutomaticPostCommitted, nil
	}
	if version < sourceUpdateAt {
		return post, knownAutomaticPostOld, nil
	}
	return nil, knownAutomaticPostMissing, errors.New("automatic known-post publication state is ambiguous")
}

func (s *Service) releaseOutcomePublicationGuard(outcome domain.AutomaticPublicationOutcome) error {
	guard, found, err := s.repository.GetAutomaticPublicationGuard(outcome.TranslationPostID)
	if err != nil {
		return fmt.Errorf("load automatic publication guard for outcome: %w", err)
	}
	if !found || guard.OperationID != outcome.OperationID || guard.LeaseUntil.IsZero() && !guard.Updating {
		return nil
	}
	released, err := s.repository.ReleaseAutomaticPublicationGuard(guard)
	if err != nil {
		return fmt.Errorf("release automatic publication guard for outcome: %w", err)
	}
	if !released {
		return ErrMappingContention
	}
	return nil
}

func (s *Service) finalizePublishedPost(job domain.AutomaticJob, source *model.Post, postID string) error {
	for range maximumMappingTransitionTries {
		published, publishedErr := s.api.GetPost(postID)
		if isMissingPost(published, publishedErr) {
			return s.deletePublishedResult(job, postID)
		}
		if publishedErr != nil {
			return errors.New("automatic publication finalization post lookup failed")
		}
		if !automaticPostMatchesOperation(published, job, s.translatorUserID) {
			return s.deletePublishedResult(job, postID)
		}
		currentSource, appErr := s.api.GetPost(job.SourcePostID)
		if isMissingPost(currentSource, appErr) {
			if err := s.cancelSourceMapping(job.SourcePostID); err != nil {
				return err
			}
			return s.deletePublishedResult(job, postID)
		}
		if appErr != nil {
			return errors.New("automatic source finalization reload failed")
		}
		if sourceVersion(currentSource) != job.SourceUpdateAt {
			return s.deletePublishedResult(job, postID)
		}

		current, token, found, err := s.repository.GetMappingForUpdate(job.SourcePostID)
		if err != nil {
			return fmt.Errorf("reload automatic mapping: %w", err)
		}
		if !found || !mappingMatchesJob(current, job) || mappingCancelled(current) {
			return s.deletePublishedResult(job, postID)
		}
		after := current
		after.ChannelID = source.ChannelId
		after.TranslationPostID = postID
		stored, err := s.repository.SaveMappingCAS(token, after)
		if err != nil {
			return fmt.Errorf("save automatic mapping: %w", err)
		}
		if stored {
			return nil
		}
	}
	if err := s.deletePublishedResult(job, postID); err != nil {
		return errors.Join(ErrMappingContention, err)
	}
	return ErrMappingContention
}

func (s *Service) deletePublishedResult(job domain.AutomaticJob, postID string) error {
	current, found, err := s.repository.GetMapping(job.SourcePostID)
	if err != nil {
		return fmt.Errorf("load mapping before orphan cleanup: %w", err)
	}
	post, appErr := s.api.GetPost(postID)
	if isMissingPost(post, appErr) {
		return nil
	}
	if appErr != nil {
		return errors.New("automatic orphan cleanup post lookup failed")
	}
	if found && current.TranslationPostID == postID && !mappingCancelled(current) {
		if automaticPostMatchesMapping(post, current, s.translatorUserID) {
			return nil
		}
		if matchesMappedAutomaticIdentity(post, current, s.translatorUserID) && current.SourceUpdateAt > job.SourceUpdateAt {
			source, sourceErr := s.api.GetPost(current.SourcePostID)
			if sourceErr != nil {
				return errors.New("automatic regressed post source lookup failed")
			}
			if source == nil || source.DeleteAt != 0 {
				return s.cancelSourceMapping(current.SourcePostID)
			}
			return s.ensureMappingJob(current, source.RootId)
		}
		return errors.New("automatic mapped post ownership could not be confirmed")
	}
	if !automaticPostMatchesOperation(post, job, s.translatorUserID) {
		postVersion, versionOK := automaticPostSourceVersion(post)
		if post.UserId == s.translatorUserID &&
			automaticPropertyEqual(post.GetProp(publication.TranslatorKindProp), "automatic") &&
			automaticPropertyEqual(post.GetProp(publication.TranslatorSourcePostIDProp), job.SourcePostID) &&
			versionOK && postVersion > job.SourceUpdateAt {
			return nil
		}
		return errors.New("automatic orphan ownership could not be confirmed")
	}
	return s.deletePublishedOperationPost(job, postID)
}

func (s *Service) deletePublishedOperationPost(job domain.AutomaticJob, postID string) (returnErr error) {
	desired := domain.AutomaticPublicationGuard{
		TranslationPostID: postID,
		SourcePostID:      job.SourcePostID,
		SourceUpdateAt:    job.SourceUpdateAt,
		OperationID:       job.OperationID,
		LeaseUntil:        s.now().UTC().Add(automaticPublicationGuardLease),
	}
	held, acquired, err := s.repository.AcquireAutomaticPublicationGuard(desired, s.now().UTC())
	if err != nil {
		return fmt.Errorf("acquire automatic compensation guard: %w", err)
	}
	if !acquired {
		return ErrMappingContention
	}
	var observed *model.Post
	defer func() {
		releaseErr := s.releaseAutomaticPublicationMutationGuard(held, observed)
		if releaseErr != nil {
			returnErr = errors.Join(returnErr, releaseErr)
		}
	}()

	post, appErr := s.api.GetPost(postID)
	if isMissingPost(post, appErr) {
		return nil
	}
	if appErr != nil {
		return errors.New("automatic compensation post lookup failed")
	}
	observed = post
	if !automaticPostMatchesOperation(post, job, s.translatorUserID) {
		return nil
	}
	updating, begun, err := s.repository.BeginAutomaticPublicationUpdate(held)
	if err != nil {
		return fmt.Errorf("begin automatic compensation fence: %w", err)
	}
	if !begun {
		held = domain.AutomaticPublicationGuard{}
		return ErrMappingContention
	}
	held = updating

	post, appErr = s.api.GetPost(postID)
	if isMissingPost(post, appErr) {
		observed = nil
		return nil
	}
	if appErr != nil {
		return errors.New("automatic fenced compensation post lookup failed")
	}
	observed = post
	if !automaticPostMatchesOperation(post, job, s.translatorUserID) {
		return nil
	}
	return s.deletePostWithIntent(postID, job.SourcePostID, job.OperationID)
}

func (s *Service) releaseAutomaticPublicationMutationGuard(
	before domain.AutomaticPublicationGuard,
	observed *model.Post,
) error {
	if before.TranslationPostID == "" {
		return nil
	}
	after := before
	after.LeaseUntil = time.Time{}
	after.Updating = false
	if observed != nil && observed.DeleteAt == 0 && observed.UserId == s.translatorUserID &&
		automaticPropertyEqual(observed.GetProp(publication.TranslatorKindProp), "automatic") &&
		automaticPropertyEqual(observed.GetProp(publication.TranslatorSourcePostIDProp), before.SourcePostID) {
		version, versionOK := automaticPostSourceVersion(observed)
		operationID := fmt.Sprint(observed.GetProp(publication.TranslatorOperationIDProp))
		if versionOK && version > after.SourceUpdateAt && operationID != "" {
			after.SourceUpdateAt = version
			after.OperationID = operationID
		}
	}
	released, err := s.repository.SaveAutomaticPublicationGuardCAS(before, after)
	if err != nil {
		return fmt.Errorf("release automatic publication mutation guard: %w", err)
	}
	if !released {
		return ErrMappingContention
	}
	return nil
}

func automaticPostMatchesMapping(post *model.Post, mapping domain.SourceMapping, translatorUserID string) bool {
	return matchesMappedAutomaticIdentity(post, mapping, translatorUserID) &&
		automaticPropertyEqual(post.GetProp(publication.TranslatorOperationIDProp), mapping.OperationID) &&
		automaticPropertyEqual(post.GetProp(publication.TranslatorSourceUpdateAtProp), mapping.SourceUpdateAt)
}

func automaticPostMatchesOperation(post *model.Post, job domain.AutomaticJob, translatorUserID string) bool {
	return post != nil && post.UserId == translatorUserID &&
		automaticPropertyEqual(post.GetProp(publication.TranslatorKindProp), "automatic") &&
		automaticPropertyEqual(post.GetProp(publication.TranslatorSourcePostIDProp), job.SourcePostID) &&
		automaticPropertyEqual(post.GetProp(publication.TranslatorOperationIDProp), job.OperationID) &&
		automaticPropertyEqual(post.GetProp(publication.TranslatorSourceUpdateAtProp), job.SourceUpdateAt)
}

func automaticPostSourceVersion(post *model.Post) (int64, bool) {
	if post == nil {
		return 0, false
	}
	version, err := strconv.ParseInt(automaticPropertyText(post.GetProp(publication.TranslatorSourceUpdateAtProp)), 10, 64)
	return version, err == nil
}

func mappingMatchesJob(mapping domain.SourceMapping, job domain.AutomaticJob) bool {
	return mapping.SourceUpdateAt == job.SourceUpdateAt && mapping.OperationID == job.OperationID
}

func mappingCancelled(mapping domain.SourceMapping) bool {
	return mapping.InternallyDeleting || mapping.SuppressedUpdateAt == mapping.SourceUpdateAt
}

func sourceVersion(post *model.Post) int64 {
	if post == nil {
		return 0
	}
	if post.UpdateAt != 0 {
		return post.UpdateAt
	}
	return post.CreateAt
}

func automaticOperationID(sourcePostID string, sourceUpdateAt int64) string {
	sum := sha256.Sum256([]byte(sourcePostID + "\x00" + strconv.FormatInt(sourceUpdateAt, 10)))
	return hex.EncodeToString(sum[:16])
}

func copyGlossary(glossary map[string]string) map[string]string {
	copied := make(map[string]string, len(glossary))
	for korean, english := range glossary {
		copied[korean] = english
	}
	return copied
}

func isMissingPost(post *model.Post, appErr *model.AppError) bool {
	return appErr != nil && appErr.StatusCode == http.StatusNotFound || appErr == nil && (post == nil || post.DeleteAt != 0)
}
