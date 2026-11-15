package main

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

// postgres changes the same rows Mattermost's own post store keeps in step
// when a reply is saved or deleted (server/channels/store/sqlstore/post_store.go,
// updateThreadsFromPosts and updateThreadAfterReplyDeletion). Channel message
// counts are left alone, as they are when a post is deleted.
type postgres struct {
	db *sql.DB
}

var errUnsupportedDatabase = errors.New("moving messages needs PostgreSQL")

type unsupported struct{}

func (unsupported) Relocate(plan) error { return errUnsupportedDatabase }

func (unsupported) ForgetEdits(string, int64) error { return errUnsupportedDatabase }

func (s postgres) ForgetEdits(postID string, since int64) error {
	_, err := s.db.Exec(`DELETE FROM Posts WHERE OriginalId = $1 AND DeleteAt >= $2`, postID, since)
	return err
}

func (s postgres) Relocate(p plan) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	now := model.GetMillis()

	ids := make([]string, len(p.Moves))
	for i, m := range p.Moves {
		ids[i] = m.ID
		result, err := tx.Exec(`UPDATE Posts SET RootId = $1, CreateAt = $2, UpdateAt = $3
			WHERE Id = $4 AND ChannelId = $5 AND DeleteAt = 0`, p.RootID, m.CreateAt, now, m.ID, p.ChannelID)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("post %s changed while moving", m.ID)
		}
	}

	// Edit history rows point at their post through OriginalId.
	in, args := list(2, ids)
	if _, err = tx.Exec(`UPDATE Posts SET RootId = $1 WHERE OriginalId IN `+in, append([]any{p.RootID}, args...)...); err != nil {
		return err
	}

	if len(p.FormerRoots) > 0 {
		in, args := list(1, p.FormerRoots)
		if _, err = tx.Exec(`DELETE FROM ThreadMemberships WHERE PostId IN `+in, args...); err != nil {
			return err
		}
		if _, err = tx.Exec(`DELETE FROM Threads WHERE PostId IN `+in, args...); err != nil {
			return err
		}
	}

	for _, root := range append([]string{p.RootID}, p.SourceRoots...) {
		if _, err = tx.Exec(`INSERT INTO Threads (PostId, ChannelId, ReplyCount, LastReplyAt, Participants, ThreadTeamId)
			SELECT $1::varchar, $2::varchar,
				(SELECT COUNT(*) FROM Posts WHERE RootId = $1 AND DeleteAt = 0),
				(SELECT COALESCE(MAX(CreateAt), 0) FROM Posts WHERE RootId = $1 AND DeleteAt = 0),
				(SELECT COALESCE(jsonb_agg(UserId ORDER BY LastAt), '[]'::jsonb) FROM
					(SELECT UserId, MAX(CreateAt) AS LastAt FROM Posts WHERE RootId = $1 AND DeleteAt = 0 GROUP BY UserId) AS replied),
				(SELECT COALESCE(TeamId, '') FROM Channels WHERE Id = $2)
			ON CONFLICT (PostId) DO UPDATE SET ReplyCount = EXCLUDED.ReplyCount,
				LastReplyAt = EXCLUDED.LastReplyAt, Participants = EXCLUDED.Participants`, root, p.ChannelID); err != nil {
			return err
		}
	}

	// Like a reply would, the move makes its authors and the root's author
	// follow the thread, already read up to now so nothing turns unread.
	for _, user := range append([]string{p.RootUserID}, p.Authors...) {
		if _, err = tx.Exec(`INSERT INTO ThreadMemberships (PostId, UserId, Following, LastViewed, LastUpdated, UnreadMentions)
			VALUES ($1, $2, true, $3, $3, 0) ON CONFLICT (PostId, UserId) DO NOTHING`, p.RootID, user, now); err != nil {
			return err
		}
	}

	// Clients that sync by UpdateAt pick up the new reply counts.
	in, args = list(2, append([]string{p.RootID}, p.SourceRoots...))
	if _, err = tx.Exec(`UPDATE Posts SET UpdateAt = $1 WHERE Id IN `+in, append([]any{now}, args...)...); err != nil {
		return err
	}
	return tx.Commit()
}

// list returns "($first, $first+1, ...)" and the matching arguments.
func list(first int, values []string) (string, []any) {
	marks := make([]string, len(values))
	args := make([]any, len(values))
	for i, value := range values {
		marks[i] = fmt.Sprintf("$%d", first+i)
		args[i] = value
	}
	return "(" + strings.Join(marks, ", ") + ")", args
}
