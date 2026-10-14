package automatic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
)

func (s *Service) ReconcileMappings(ctx context.Context) error {
	if s == nil || s.scope == nil || s.repository == nil || s.api == nil || s.publisher == nil {
		return errors.New("automatic service is unavailable")
	}
	if err := s.reconcilePublicationGuards(); err != nil {
		return err
	}
	outcomes, err := s.repository.ListAutomaticPublicationOutcomes()
	if err != nil {
		return fmt.Errorf("list automatic publication outcomes: %w", err)
	}
	for _, outcome := range outcomes {
		job := domain.AutomaticJob{
			SourcePostID:       outcome.SourcePostID,
			SourceUpdateAt:     outcome.SourceUpdateAt,
			ChannelID:          outcome.ChannelID,
			OperationID:        outcome.OperationID,
			OperationStartedAt: outcome.OperationStartedAt,
		}
		if _, err := s.resolvePublicationOutcome(ctx, job, outcome); err != nil {
			return err
		}
	}
	mappings, err := s.repository.ListMappings()
	if err != nil {
		return fmt.Errorf("list automatic mappings: %w", err)
	}
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].SourcePostID < mappings[j].SourcePostID })
	for _, mapping := range mappings {
		if err := s.reconcileMapping(ctx, mapping); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) reconcilePublicationGuards() error {
	guards, err := s.repository.ListAutomaticPublicationGuards()
	if err != nil {
		return fmt.Errorf("list automatic publication guards: %w", err)
	}
	for _, guard := range guards {
		if !guard.Updating {
			continue
		}
		post, appErr := s.api.GetPost(guard.TranslationPostID)
		if appErr != nil && appErr.StatusCode != http.StatusNotFound {
			return errors.New("automatic publication guard post lookup failed")
		}
		after := guard
		after.LeaseUntil = time.Time{}
		after.Updating = false
		if appErr == nil && post != nil && post.DeleteAt == 0 {
			if post.UserId != s.translatorUserID ||
				!automaticPropertyEqual(post.GetProp(publication.TranslatorKindProp), "automatic") ||
				!automaticPropertyEqual(post.GetProp(publication.TranslatorSourcePostIDProp), guard.SourcePostID) {
				return errors.New("automatic publication guard post identity changed")
			}
			postVersion, versionOK := automaticPostSourceVersion(post)
			postOperation := fmt.Sprint(post.GetProp(publication.TranslatorOperationIDProp))
			if !versionOK || postOperation == "" {
				return errors.New("automatic publication guard post metadata is incomplete")
			}
			if postVersion == guard.SourceUpdateAt && postOperation != guard.OperationID {
				return errors.New("automatic publication guard post operation changed at the guarded version")
			}
			if postVersion > guard.SourceUpdateAt {
				after.SourceUpdateAt = postVersion
				after.OperationID = postOperation
			}
		}
		released, err := s.repository.SaveAutomaticPublicationGuardCAS(guard, after)
		if err != nil {
			return fmt.Errorf("recover automatic publication guard: %w", err)
		}
		if !released {
			return ErrMappingContention
		}
	}
	return nil
}

func (s *Service) reconcileMapping(ctx context.Context, mapping domain.SourceMapping) error {
	source, appErr := s.api.GetPost(mapping.SourcePostID)
	if isMissingPost(source, appErr) {
		return s.cancelSourceMapping(mapping.SourcePostID)
	}
	if appErr != nil {
		return errors.New("automatic source reconciliation lookup failed")
	}
	version := sourceVersion(source)
	if version != mapping.SourceUpdateAt {
		eligible, err := s.scope.Eligible(source, 0)
		if err != nil {
			return err
		}
		if !eligible {
			return nil
		}
		_, err = s.enqueueSource(source)
		return err
	}
	if mappingCancelled(mapping) {
		return nil
	}
	if mapping.TranslationPostID == mapping.SourcePostID {
		if _, owned := publication.StripInlineTranslation(source); owned {
			return nil
		}
		eligible, err := s.scope.Eligible(source, 0)
		if err != nil || !eligible {
			return err
		}
		return s.ensureMappingJob(mapping, source.RootId)
	}
	if mapping.TranslationPostID != "" {
		_, state, err := s.inspectKnownAutomaticPost(
			mapping.TranslationPostID,
			mapping.SourcePostID,
			mapping.ChannelID,
			mapping.OperationID,
			mapping.SourceUpdateAt,
		)
		if err != nil {
			return err
		}
		switch state {
		case knownAutomaticPostCommitted:
			return nil
		case knownAutomaticPostOld:
			eligible, err := s.scope.Eligible(source, 0)
			if err != nil {
				return err
			}
			if !eligible {
				return nil
			}
			return s.ensureMappingJob(mapping, source.RootId)
		}
	}

	result, err := s.publisher.Reconcile(ctx, publication.Operation{
		ID:        mapping.OperationID,
		StartedAt: mapping.OperationStartedAt,
		ChannelID: mapping.ChannelID,
		UserID:    s.translatorUserID,
	})
	if err == nil {
		if result.PostID == "" {
			return errors.New("automatic reconciliation returned no post")
		}
		return s.adoptReconciledPost(mapping, result.PostID)
	}
	if !errors.Is(err, publication.ErrReconcileUncertain) {
		return err
	}
	eligible, eligibleErr := s.scope.Eligible(source, 0)
	if eligibleErr != nil {
		return eligibleErr
	}
	if !eligible {
		return nil
	}

	if mapping.TranslationPostID != "" {
		translation, lookupErr := s.api.GetPost(mapping.TranslationPostID)
		if lookupErr != nil && lookupErr.StatusCode != http.StatusNotFound {
			return errors.New("automatic translation reconciliation lookup failed")
		}
		if lookupErr == nil && translation != nil && translation.DeleteAt == 0 {
			if matchesMappedAutomaticIdentity(translation, mapping, s.translatorUserID) &&
				(!automaticPropertyEqual(translation.GetProp(publication.TranslatorOperationIDProp), mapping.OperationID) ||
					!automaticPropertyEqual(translation.GetProp(publication.TranslatorSourceUpdateAtProp), mapping.SourceUpdateAt)) {
				return s.ensureMappingJob(mapping, source.RootId)
			}
			return err
		}
	}
	return s.ensureMappingJob(mapping, source.RootId)
}

func matchesMappedAutomaticIdentity(post *model.Post, mapping domain.SourceMapping, translatorUserID string) bool {
	return post != nil && post.UserId == translatorUserID && post.ChannelId == mapping.ChannelID &&
		automaticPropertyEqual(post.GetProp(publication.TranslatorKindProp), "automatic") &&
		automaticPropertyEqual(post.GetProp(publication.TranslatorSourcePostIDProp), mapping.SourcePostID)
}

func automaticPropertyEqual(left, right any) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return automaticPropertyText(left) == automaticPropertyText(right)
}

func automaticPropertyText(value any) string {
	if number, ok := value.(float64); ok {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	return fmt.Sprint(value)
}

func (s *Service) adoptReconciledPost(mapping domain.SourceMapping, postID string) error {
	for range maximumMappingTransitionTries {
		current, token, found, err := s.repository.GetMappingForUpdate(mapping.SourcePostID)
		if err != nil {
			return fmt.Errorf("reload reconciled automatic mapping: %w", err)
		}
		if !found || current.SourceUpdateAt != mapping.SourceUpdateAt || current.OperationID != mapping.OperationID || mappingCancelled(current) {
			return nil
		}
		after := current
		after.TranslationPostID = postID
		stored, err := s.repository.SaveMappingCAS(token, after)
		if err != nil {
			return fmt.Errorf("save reconciled automatic mapping: %w", err)
		}
		if stored {
			return nil
		}
	}
	return ErrMappingContention
}

func (s *Service) ensureMappingJob(mapping domain.SourceMapping, rootID string) error {
	existing, found, err := s.repository.GetJob(mapping.SourcePostID, mapping.SourceUpdateAt)
	if err != nil {
		return fmt.Errorf("load reconciled automatic job: %w", err)
	}
	job := domain.AutomaticJob{
		SourcePostID:       mapping.SourcePostID,
		SourceUpdateAt:     mapping.SourceUpdateAt,
		ChannelID:          mapping.ChannelID,
		RootID:             rootID,
		OperationID:        mapping.OperationID,
		OperationStartedAt: mapping.OperationStartedAt,
		State:              domain.JobPending,
	}
	if found && existing.OperationID == mapping.OperationID {
		switch existing.State {
		case domain.JobPending, domain.JobProcessing, domain.JobRetryableFailed:
			s.wake()
			return nil
		}
	}
	if err := s.repository.PutJob(job); err != nil {
		return fmt.Errorf("persist reconciled automatic job: %w", err)
	}
	s.wake()
	return nil
}
