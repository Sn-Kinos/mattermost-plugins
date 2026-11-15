package command_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/apartmentlines/mattermost-plugin-poor-mans-scheduled-messages/internal/ports"
	"github.com/apartmentlines/mattermost-plugin-poor-mans-scheduled-messages/server/types"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	delUserID  = "owner-user-id"
	delChannel = "dm-channel-id"
	delLink    = "in direct message with: @someone"
	delIDFirst = "aaaa1111-0000-4000-8000-000000000001"
	delIDLater = "bbbb2222-0000-4000-8000-000000000002"
)

func deleteArgs(text string) *model.CommandArgs {
	return &model.CommandArgs{UserId: delUserID, ChannelId: delChannel, Command: "/schedule " + text}
}

func storedMessages() map[string]*types.ScheduledMessage {
	return map[string]*types.ScheduledMessage{
		delIDFirst: {ID: delIDFirst, UserID: delUserID, ChannelID: delChannel, PostAt: time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), MessageContent: "first", Timezone: "UTC"},
		delIDLater: {ID: delIDLater, UserID: delUserID, ChannelID: delChannel, PostAt: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), MessageContent: "later", Timezone: "Asia/Seoul"},
	}
}

func expectUserMessages(mocks *testMocks, msgs map[string]*types.ScheduledMessage, order ...string) {
	mocks.store.EXPECT().ListUserMessageIDs(delUserID).Return(order, nil)
	for _, id := range order {
		mocks.store.EXPECT().GetScheduledMessage(id).Return(msgs[id], nil).AnyTimes()
	}
}

func expectConfirmationLink(mocks *testMocks) {
	info := &ports.ChannelInfo{ChannelID: delChannel, ChannelType: model.ChannelTypeDirect}
	mocks.channel.EXPECT().GetInfoOrUnknown(delChannel).Return(info)
	mocks.channel.EXPECT().MakeChannelLink(info).Return(delLink)
}

func TestExecute_DeleteByListNumber(t *testing.T) {
	handler, mocks, ctrl := setup(t)
	defer ctrl.Finish()
	expectUserMessages(mocks, storedMessages(), delIDLater, delIDFirst)
	mocks.store.EXPECT().DeleteScheduledMessage(delUserID, delIDLater).Return(nil)
	expectConfirmationLink(mocks)

	resp, appErr := handler.Execute(deleteArgs("delete 2"))

	require.Nil(t, appErr)
	assert.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
	assert.Equal(t, "✅ Message scheduled for **Nov 17, 2026 9:00 AM** in direct message with: @someone has been deleted.", resp.Text)
}

func TestExecute_DeleteByShortID(t *testing.T) {
	handler, mocks, ctrl := setup(t)
	defer ctrl.Finish()
	expectUserMessages(mocks, storedMessages(), delIDLater, delIDFirst)
	mocks.store.EXPECT().DeleteScheduledMessage(delUserID, delIDFirst).Return(nil)
	expectConfirmationLink(mocks)

	resp, appErr := handler.Execute(deleteArgs("delete aaaa1111"))

	require.Nil(t, appErr)
	assert.Equal(t, "✅ Message scheduled for **Nov 16, 2026 12:00 AM** in direct message with: @someone has been deleted.", resp.Text)
}

func TestExecute_DeleteByFullID(t *testing.T) {
	handler, mocks, ctrl := setup(t)
	defer ctrl.Finish()
	expectUserMessages(mocks, storedMessages(), delIDLater, delIDFirst)
	mocks.store.EXPECT().DeleteScheduledMessage(delUserID, delIDLater).Return(nil)
	expectConfirmationLink(mocks)

	resp, appErr := handler.Execute(deleteArgs("delete " + delIDLater))

	require.Nil(t, appErr)
	assert.Contains(t, resp.Text, "has been deleted.")
}

func TestExecute_DeleteRejectsUnknownOrAmbiguousRefs(t *testing.T) {
	sameShortA := "cccc3333-0000-4000-8000-00000000000a"
	sameShortB := "cccc3333-0000-4000-8000-00000000000b"
	cases := []struct {
		name string
		text string
		want string
	}{
		{"number past the end", "delete 3", "❌ No scheduled message #3. Run `/schedule list` to see the numbers."},
		{"number zero", "delete 0", "❌ No scheduled message #0. Run `/schedule list` to see the numbers."},
		{"unknown id", "delete dddd4444", "❌ No scheduled message matches id `dddd4444`. Run `/schedule list` to see the ids."},
		{"ambiguous id", "delete cccc3333", "❌ Id `cccc3333` matches 2 scheduled messages. Use more of the id or the list number."},
		{"too short id", "delete abc", "❌ Use the list number or at least 8 characters of the id, e.g. `/schedule delete 1`."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, mocks, ctrl := setup(t)
			defer ctrl.Finish()
			msgs := storedMessages()
			msgs[sameShortA] = &types.ScheduledMessage{ID: sameShortA, UserID: delUserID, ChannelID: delChannel, PostAt: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC), Timezone: "UTC"}
			msgs[sameShortB] = &types.ScheduledMessage{ID: sameShortB, UserID: delUserID, ChannelID: delChannel, PostAt: time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC), Timezone: "UTC"}
			if tc.name == "number past the end" || tc.name == "number zero" {
				expectUserMessages(mocks, msgs, delIDLater, delIDFirst)
			} else if tc.name != "too short id" {
				expectUserMessages(mocks, msgs, delIDLater, delIDFirst, sameShortA, sameShortB)
			}

			resp, appErr := handler.Execute(deleteArgs(tc.text))

			require.Nil(t, appErr)
			assert.Equal(t, model.CommandResponseTypeEphemeral, resp.ResponseType)
			assert.Equal(t, tc.want, resp.Text)
		})
	}
}

func TestExecute_DeleteWithoutArgumentShowsUsage(t *testing.T) {
	handler, _, ctrl := setup(t)
	defer ctrl.Finish()

	resp, appErr := handler.Execute(deleteArgs("delete"))

	require.Nil(t, appErr)
	assert.Equal(t, "❌ Usage: `/schedule delete <number|id>`. Run `/schedule list` to see the numbers and ids.", resp.Text)
}

func TestExecute_DeleteKeepsOwnerCheck(t *testing.T) {
	handler, mocks, ctrl := setup(t)
	defer ctrl.Finish()
	msgs := storedMessages()
	msgs[delIDFirst].UserID = "someone-else"
	expectUserMessages(mocks, msgs, delIDFirst)

	resp, appErr := handler.Execute(deleteArgs("delete 1"))

	require.Nil(t, appErr)
	assert.Equal(t, "❌ Could not delete message: user owner-user-id attempted to delete message "+delIDFirst+" owned by someone-else", resp.Text)
}

func TestExecute_DeleteReportsStoreFailure(t *testing.T) {
	handler, mocks, ctrl := setup(t)
	defer ctrl.Finish()
	expectUserMessages(mocks, storedMessages(), delIDFirst)
	mocks.store.EXPECT().DeleteScheduledMessage(delUserID, delIDFirst).Return(errors.New("kv down"))

	resp, appErr := handler.Execute(deleteArgs("delete 1"))

	require.Nil(t, appErr)
	assert.Equal(t, "❌ Could not delete message: failed to delete scheduled message "+delIDFirst+": kv down", resp.Text)
}

// A record exactly as v1.3.1 writes it to the KV store (types.ScheduledMessage JSON),
// with the UTC timezone the bot account produced before the configurable default.
const v131StoredRecord = `{"id":"0f6a9c2e-5b1d-4e2a-9c7b-3d8e1f2a4b5c","user_id":"owner-user-id","channel_id":"dm-channel-id","post_at":"2026-11-16T09:00:00Z","message_content":"hello from v1.3.1","timezone":"UTC"}`

func TestExecute_DeleteReadsV131Record(t *testing.T) {
	handler, mocks, ctrl := setup(t)
	defer ctrl.Finish()
	var stored types.ScheduledMessage
	require.NoError(t, json.Unmarshal([]byte(v131StoredRecord), &stored))
	expectUserMessages(mocks, map[string]*types.ScheduledMessage{stored.ID: &stored}, stored.ID)
	mocks.store.EXPECT().DeleteScheduledMessage(delUserID, stored.ID).Return(nil)
	expectConfirmationLink(mocks)

	resp, appErr := handler.Execute(deleteArgs("delete 0f6a9c2e"))

	require.Nil(t, appErr)
	assert.Equal(t, "✅ Message scheduled for **Nov 16, 2026 9:00 AM** in direct message with: @someone has been deleted.", resp.Text)
}
