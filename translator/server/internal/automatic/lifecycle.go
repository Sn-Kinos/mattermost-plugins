package automatic

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/publication"
)

const automaticDeletionIntentLifetime = int64(300)

func (s *Service) HandlePosted(post *model.Post) error {
	return s.handleSource(post, s.activationAt)
}

func (s *Service) HandleUpdated(post *model.Post) error {
	if s == nil || s.repository == nil || post == nil {
		return nil
	}
	_, mapped, err := s.repository.GetMapping(post.Id)
	if err != nil {
		return fmt.Errorf("load updated source mapping: %w", err)
	}
	activationAt := s.activationAt
	if mapped {
		activationAt = 0
	}
	return s.handleSource(post, activationAt)
}

func (s *Service) HandleDeleted(post *model.Post) error {
	if s == nil || s.repository == nil || s.api == nil || post == nil {
		return nil
	}
	if post.UserId == s.translatorUserID {
		return s.handleTranslationDeleted(post)
	}
	return s.cancelSourceMapping(post.Id)
}

func (s *Service) handleSource(post *model.Post, activationAt int64) error {
	if s == nil || s.scope == nil || s.repository == nil || post == nil {
		return nil
	}
	eligible, err := s.scope.Eligible(post, activationAt)
	if err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	_, err = s.enqueueSource(post)
	return err
}

func (s *Service) enqueueSource(post *model.Post) (bool, error) {
	version := sourceVersion(post)
	existingJob, jobFound, err := s.repository.GetJob(post.Id, version)
	if err != nil {
		return false, fmt.Errorf("load automatic job: %w", err)
	}
	startedNow := s.now().UTC()
	for range maximumMappingTransitionTries {
		mapping, token, mappingFound, err := s.repository.GetMappingForUpdate(post.Id)
		if err != nil {
			return false, fmt.Errorf("load automatic mapping: %w", err)
		}
		if mappingFound && mapping.SourceUpdateAt > version {
			return false, nil
		}
		if jobFound && mappingFound && mapping.SourceUpdateAt == version && mapping.OperationID == existingJob.OperationID {
			return false, nil
		}

		operationID := automaticOperationID(post.Id, version)
		operationStartedAt := startedNow
		if mappingFound && mapping.SourceUpdateAt == version && mapping.OperationID != "" {
			operationID = mapping.OperationID
			operationStartedAt = mapping.OperationStartedAt
		} else if jobFound {
			operationID = existingJob.OperationID
			operationStartedAt = existingJob.OperationStartedAt
		}

		translationPostID := ""
		if mappingFound {
			translationPostID = mapping.TranslationPostID
			if version != mapping.SourceUpdateAt && mapping.SuppressedUpdateAt == mapping.SourceUpdateAt {
				translationPostID = ""
			}
		}
		updatedMapping := domain.SourceMapping{
			SourcePostID:       post.Id,
			SourceUpdateAt:     version,
			ChannelID:          post.ChannelId,
			OperationID:        operationID,
			OperationStartedAt: operationStartedAt,
			TranslationPostID:  translationPostID,
		}
		if mappingFound && version == mapping.SourceUpdateAt {
			updatedMapping.SuppressedUpdateAt = mapping.SuppressedUpdateAt
			updatedMapping.InternallyDeleting = mapping.InternallyDeleting
		}

		var stored bool
		if mappingFound {
			stored, err = s.repository.SaveMappingCAS(token, updatedMapping)
		} else {
			stored, err = s.repository.CreateMapping(updatedMapping)
		}
		if err != nil {
			return false, fmt.Errorf("persist automatic mapping: %w", err)
		}
		if !stored {
			continue
		}
		if !jobFound || existingJob.OperationID != operationID {
			job := domain.AutomaticJob{
				SourcePostID:       post.Id,
				SourceUpdateAt:     version,
				ChannelID:          post.ChannelId,
				RootID:             post.RootId,
				OperationID:        operationID,
				OperationStartedAt: operationStartedAt,
				State:              domain.JobPending,
			}
			if err := s.repository.PutJob(job); err != nil {
				return false, fmt.Errorf("persist automatic job: %w", err)
			}
		}
		s.wake()
		return true, nil
	}
	return false, ErrMappingContention
}

func (s *Service) handleTranslationDeleted(post *model.Post) error {
	consumed, err := s.repository.ConsumeDeletionIntent(post.Id)
	if err != nil {
		return fmt.Errorf("consume automatic deletion intent: %w", err)
	}
	for range maximumMappingTransitionTries {
		mapping, token, found, err := s.mappingForTranslation(post)
		if err != nil {
			return err
		}
		if !found || mapping.TranslationPostID != post.Id {
			return nil
		}
		after := mapping
		after.TranslationPostID = ""
		if !consumed && !mapping.InternallyDeleting {
			after.SuppressedUpdateAt = mapping.SourceUpdateAt
		}
		stored, err := s.repository.SaveMappingCAS(token, after)
		if err != nil {
			return fmt.Errorf("persist automatic translation deletion: %w", err)
		}
		if stored {
			return nil
		}
	}
	return ErrMappingContention
}

func (s *Service) mappingForTranslation(post *model.Post) (domain.SourceMapping, string, bool, error) {
	if sourceID, _ := post.GetProp(publication.TranslatorSourcePostIDProp).(string); sourceID != "" {
		mapping, token, found, err := s.repository.GetMappingForUpdate(sourceID)
		if err != nil {
			return domain.SourceMapping{}, "", false, fmt.Errorf("load automatic translation mapping: %w", err)
		}
		if found && mapping.TranslationPostID == post.Id {
			return mapping, token, true, nil
		}
	}
	mappings, err := s.repository.ListMappings()
	if err != nil {
		return domain.SourceMapping{}, "", false, fmt.Errorf("list automatic mappings: %w", err)
	}
	for _, candidate := range mappings {
		if candidate.TranslationPostID == post.Id {
			mapping, token, found, err := s.repository.GetMappingForUpdate(candidate.SourcePostID)
			if err != nil {
				return domain.SourceMapping{}, "", false, fmt.Errorf("reload automatic translation mapping: %w", err)
			}
			if found && mapping.TranslationPostID == post.Id {
				return mapping, token, true, nil
			}
		}
	}
	return domain.SourceMapping{}, "", false, nil
}

func (s *Service) cancelSourceMapping(sourcePostID string) error {
	for range maximumMappingTransitionTries {
		mapping, token, found, err := s.repository.GetMappingForUpdate(sourcePostID)
		if err != nil {
			return fmt.Errorf("load deleted source mapping: %w", err)
		}
		if !found {
			return nil
		}
		if !mapping.InternallyDeleting {
			after := mapping
			after.InternallyDeleting = true
			stored, err := s.repository.SaveMappingCAS(token, after)
			if err != nil {
				return fmt.Errorf("mark automatic mapping for deletion: %w", err)
			}
			if !stored {
				continue
			}
			continue
		}
		if mapping.TranslationPostID == "" {
			return nil
		}
		if mapping.TranslationPostID == mapping.SourcePostID {
			after := mapping
			after.TranslationPostID = ""
			stored, err := s.repository.SaveMappingCAS(token, after)
			if err != nil {
				return fmt.Errorf("finish automatic inline source cleanup: %w", err)
			}
			if stored {
				return nil
			}
			continue
		}
		return s.deleteInternalTranslation(mapping)
	}
	return ErrMappingContention
}

func (s *Service) deleteInternalTranslation(mapping domain.SourceMapping) error {
	translationPostID := mapping.TranslationPostID
	if err := s.deletePostWithIntent(translationPostID, mapping.SourcePostID, mapping.OperationID); err != nil {
		return err
	}
	for range maximumMappingTransitionTries {
		current, token, found, err := s.repository.GetMappingForUpdate(mapping.SourcePostID)
		if err != nil {
			return fmt.Errorf("reload deleted automatic mapping: %w", err)
		}
		if !found || current.TranslationPostID != translationPostID || !current.InternallyDeleting {
			return nil
		}
		after := current
		after.TranslationPostID = ""
		stored, err := s.repository.SaveMappingCAS(token, after)
		if err != nil {
			return fmt.Errorf("finish automatic source cleanup: %w", err)
		}
		if stored {
			return nil
		}
	}
	return ErrMappingContention
}

func (s *Service) deletePostWithIntent(postID, sourcePostID, operationID string) error {
	if postID == "" {
		return nil
	}
	intent := domain.DeletionIntent{
		TranslationPostID: postID,
		SourcePostID:      sourcePostID,
		OperationID:       operationID,
	}
	if err := s.repository.PutDeletionIntent(intent, automaticDeletionIntentLifetime); err != nil {
		return fmt.Errorf("persist automatic deletion intent: %w", err)
	}
	if appErr := s.api.DeletePost(postID); appErr != nil && appErr.StatusCode != http.StatusNotFound {
		return errors.New("delete automatic translation failed")
	}
	return nil
}

func (s *Service) wake() {
	if s.waker != nil {
		s.waker.Wake()
	}
}
