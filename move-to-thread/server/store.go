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

func (unsupported) NewestRoot(string) (string, error) { return "", errUnsupportedDatabase }

func (s postgres) ForgetEdits(postID string, since int64) error {
	_, err := s.db.Exec(`DELETE FROM Posts WHERE OriginalId = $1 AND DeleteAt >= $2`, postID, since)
	return err
}

func (s postgres) NewestRoot(channelID string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT Id FROM Posts WHERE ChannelId = $1 AND RootId = '' AND DeleteAt = 0
		AND Type NOT LIKE 'system\_%' AND Type <> $2 ORDER BY CreateAt DESC LIMIT 1`, channelID, model.PostTypeBurnOnRead).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// postChannelTables hold a copy of their post's channel.
var postChannelTables = []struct{ table, postID, where string }{
	{"FileInfo", "PostId", ""},
	{"Reactions", "PostId", ""},
	{"PostAcknowledgements", "PostId", ""},
	{"PostsPriority", "PostId", ""},
	{"Translations", "ObjectId", " AND t.ObjectType = 'post'"},
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

	if p.RootID != "" {
		var live bool
		if err = tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM Posts WHERE Id = $1 AND ChannelId = $2 AND RootId = '' AND DeleteAt = 0 FOR UPDATE)`,
			p.RootID, p.To).Scan(&live); err != nil {
			return err
		}
		if !live {
			return fmt.Errorf("thread %s is gone", p.RootID)
		}
	}

	ids := make([]string, len(p.Moves))
	for i, m := range p.Moves {
		ids[i] = m.ID
		result, err := tx.Exec(`UPDATE Posts SET RootId = $1, CreateAt = $2, UpdateAt = $3, ChannelId = $4
			WHERE Id = $5 AND ChannelId = $6 AND DeleteAt = 0`, m.RootID, m.CreateAt, now, p.To, m.ID, p.From)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("post %s changed while moving", m.ID)
		}
	}

	// Edit history rows point at their post through OriginalId.
	in, args := list(1, ids)
	if _, err = tx.Exec(`UPDATE Posts AS history SET RootId = post.RootId, ChannelId = post.ChannelId
		FROM Posts AS post WHERE history.OriginalId = post.Id AND post.Id IN `+in, args...); err != nil {
		return err
	}

	if len(p.FormerRoots) > 0 {
		// Replies posted to a former root since the plan was made, and its
		// deleted replies, follow it so no reply is left under a reply.
		in, args := list(4, p.FormerRoots)
		if _, err = tx.Exec(`UPDATE Posts SET RootId = $1, ChannelId = $2, UpdateAt = $3 WHERE RootId IN `+in,
			append([]any{p.RootID, p.To, now}, args...)...); err != nil {
			return err
		}
		in, args = list(1, p.FormerRoots)
		if _, err = tx.Exec(`DELETE FROM ThreadMemberships WHERE PostId IN `+in, args...); err != nil {
			return err
		}
		if _, err = tx.Exec(`DELETE FROM Threads WHERE PostId IN `+in, args...); err != nil {
			return err
		}
	}

	if len(p.MovedRoots) > 0 {
		// Likewise every reply of a root that changes channel goes with it.
		in, args := list(4, p.MovedRoots)
		if _, err = tx.Exec(`UPDATE Posts SET ChannelId = $1, UpdateAt = $2 WHERE ChannelId = $3 AND RootId IN `+in,
			append([]any{p.To, now, p.From}, args...)...); err != nil {
			return err
		}
		in, args = list(3, p.MovedRoots)
		if _, err = tx.Exec(`UPDATE Threads SET ChannelId = $1, ThreadTeamId = $2 WHERE PostId IN `+in,
			append([]any{p.To, p.ToTeamID}, args...)...); err != nil {
			return err
		}
		in, args = list(2, p.MovedRoots)
		if _, err = tx.Exec(`DELETE FROM ThreadMemberships AS m WHERE m.PostId IN `+in+`
			AND NOT EXISTS (SELECT 1 FROM ChannelMembers WHERE ChannelId = $1 AND UserId = m.UserId)`,
			append([]any{p.To}, args...)...); err != nil {
			return err
		}
	}

	if roots := append(p.FormerRoots, p.MovedRoots...); p.From != p.To && len(roots) > 0 {
		// Unsent and scheduled replies follow the thread.
		in, args := list(3, roots)
		for _, table := range []string{"Drafts", "ScheduledPosts"} {
			if _, err = tx.Exec(`UPDATE `+table+` SET ChannelId = $1 WHERE ChannelId = $2 AND RootId IN `+in,
				append([]any{p.To, p.From}, args...)...); err != nil {
				return err
			}
		}
	}

	if p.From != p.To {
		for _, t := range postChannelTables {
			if _, err = tx.Exec(`UPDATE `+t.table+` AS t SET ChannelId = post.ChannelId FROM Posts AS post
				WHERE t.`+t.postID+` = post.Id AND t.ChannelId = $1 AND post.ChannelId = $2`+t.where, p.From, p.To); err != nil {
				return err
			}
		}
	}

	type thread struct{ root, channelID string }
	recount := make([]thread, 0, len(p.SourceRoots)+1)
	if p.RootID != "" {
		recount = append(recount, thread{p.RootID, p.To})
	}
	for _, root := range p.SourceRoots {
		recount = append(recount, thread{root, p.From})
	}
	for _, t := range recount {
		if _, err = tx.Exec(`INSERT INTO Threads (PostId, ChannelId, ReplyCount, LastReplyAt, Participants, ThreadTeamId)
			SELECT $1::varchar, $2::varchar,
				(SELECT COUNT(*) FROM Posts WHERE RootId = $1 AND DeleteAt = 0),
				(SELECT COALESCE(MAX(CreateAt), 0) FROM Posts WHERE RootId = $1 AND DeleteAt = 0),
				(SELECT COALESCE(jsonb_agg(UserId ORDER BY LastAt), '[]'::jsonb) FROM
					(SELECT UserId, MAX(CreateAt) AS LastAt FROM Posts WHERE RootId = $1 AND DeleteAt = 0 GROUP BY UserId) AS replied),
				(SELECT COALESCE(TeamId, '') FROM Channels WHERE Id = $2)
			ON CONFLICT (PostId) DO UPDATE SET ReplyCount = EXCLUDED.ReplyCount,
				LastReplyAt = EXCLUDED.LastReplyAt, Participants = EXCLUDED.Participants,
				ChannelId = EXCLUDED.ChannelId, ThreadTeamId = EXCLUDED.ThreadTeamId`, t.root, t.channelID); err != nil {
			return err
		}
	}

	if p.RootID != "" {
		// Like a reply would, the move makes its authors and the root's
		// author follow the thread, already read up to now so nothing turns
		// unread. Authors from another channel follow only if they can see it.
		for _, user := range append([]string{p.RootUserID}, p.Authors...) {
			if _, err = tx.Exec(`INSERT INTO ThreadMemberships (PostId, UserId, Following, LastViewed, LastUpdated, UnreadMentions)
				SELECT $1::varchar, $2::varchar, true, $3::bigint, $3::bigint, 0
				WHERE $4::varchar = $5::varchar OR EXISTS (SELECT 1 FROM ChannelMembers WHERE ChannelId = $5 AND UserId = $2)
				ON CONFLICT (PostId, UserId) DO NOTHING`, p.RootID, user, now, p.From, p.To); err != nil {
				return err
			}
		}
	}

	// Clients that sync by UpdateAt pick up the new reply counts.
	if len(recount) > 0 {
		roots := make([]string, len(recount))
		for i, t := range recount {
			roots[i] = t.root
		}
		in, args = list(2, roots)
		if _, err = tx.Exec(`UPDATE Posts SET UpdateAt = $1 WHERE Id IN `+in, append([]any{now}, args...)...); err != nil {
			return err
		}
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
