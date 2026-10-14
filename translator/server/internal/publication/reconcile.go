package publication

import (
	"context"
	"errors"
	"sort"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/Sn-Kinos/mattermost-plugins/translator/server/internal/domain"
)

const (
	channelScanPageSize    = 200
	deletionIntentLifetime = int64(300)
)

func (p *Publisher) Reconcile(ctx context.Context, operation Operation) (Result, error) {
	if err := validateOperation(operation); err != nil {
		return Result{}, err
	}
	matches, err := p.scan(ctx, operation)
	if err != nil {
		return Result{}, err
	}
	if len(matches) == 0 {
		return Result{}, errors.Join(ErrReconcileUncertain, ErrOperationPostNotFound)
	}
	return p.adoptMatches(ctx, operation, matches)
}

func (p *Publisher) scan(ctx context.Context, operation Operation) ([]*model.Post, error) {
	if p == nil || p.api == nil {
		return nil, uncertain("publication API is unavailable", nil)
	}
	startedAt := operation.StartedAt.UnixMilli()
	matches := make([]*model.Post, 0, 1)
	for page := 0; ; page++ {
		if err := contextError(ctx); err != nil {
			return nil, err
		}
		list, appErr := p.api.GetPostsForChannel(operation.ChannelID, page, channelScanPageSize)
		if appErr != nil {
			return nil, uncertain("channel scan failed", appErr)
		}
		if list == nil {
			return nil, uncertain("channel scan returned no result", nil)
		}

		posts := orderedPosts(list)
		if len(posts) == 0 {
			break
		}
		oldest := posts[0].CreateAt
		for _, post := range posts {
			if post == nil {
				continue
			}
			if post.CreateAt < oldest {
				oldest = post.CreateAt
			}
			if post.DeleteAt == 0 && post.UserId == operation.UserID &&
				propEqual(post.GetProp(TranslatorOperationIDProp), operation.ID) {
				matches = append(matches, post.Clone())
			}
		}
		if oldest < startedAt || len(posts) < channelScanPageSize {
			break
		}
	}
	return matches, nil
}

func orderedPosts(list *model.PostList) []*model.Post {
	posts := make([]*model.Post, 0, len(list.Posts))
	seen := make(map[string]struct{}, len(list.Posts))
	for _, id := range list.Order {
		post := list.Posts[id]
		if post == nil {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		posts = append(posts, post)
	}
	remainingIDs := make([]string, 0, len(list.Posts)-len(posts))
	for id, post := range list.Posts {
		if post == nil {
			continue
		}
		if _, present := seen[id]; !present {
			remainingIDs = append(remainingIDs, id)
		}
	}
	sort.Strings(remainingIDs)
	for _, id := range remainingIDs {
		posts = append(posts, list.Posts[id])
	}
	return posts
}

func (p *Publisher) adoptMatches(ctx context.Context, operation Operation, matches []*model.Post) (Result, error) {
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].CreateAt == matches[j].CreateAt {
			return matches[i].Id < matches[j].Id
		}
		return matches[i].CreateAt < matches[j].CreateAt
	})
	winner := matches[0]
	for _, duplicate := range matches[1:] {
		if err := contextError(ctx); err != nil {
			return Result{}, err
		}
		if p.deletionIntents == nil {
			return Result{}, uncertain("deletion intent repository is unavailable", nil)
		}
		intent := domain.DeletionIntent{
			TranslationPostID: duplicate.Id,
			SourcePostID:      propString(duplicate.GetProp(TranslatorSourcePostIDProp)),
			OperationID:       operation.ID,
		}
		if err := p.deletionIntents.PutDeletionIntent(intent, deletionIntentLifetime); err != nil {
			return Result{}, uncertain("duplicate deletion intent could not be persisted", err)
		}
		if appErr := p.api.DeletePost(duplicate.Id); appErr != nil {
			return Result{}, uncertain("duplicate post could not be deleted", appErr)
		}
	}
	return resultForPost(winner), nil
}

func propString(value any) string {
	if value == nil {
		return ""
	}
	text, _ := value.(string)
	return text
}
