// Package command provides slash command handling.
package command

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/apartmentlines/mattermost-plugin-poor-mans-scheduled-messages/internal/ports"
	"github.com/apartmentlines/mattermost-plugin-poor-mans-scheduled-messages/server/constants"
	"github.com/apartmentlines/mattermost-plugin-poor-mans-scheduled-messages/server/formatter"
	"github.com/apartmentlines/mattermost-plugin-poor-mans-scheduled-messages/server/types"
	"github.com/mattermost/mattermost/server/public/model"
)

// Handler coordinates slash command execution.
type Handler struct {
	logger          ports.Logger
	slasher         ports.SlashCommandService
	user            ports.UserService
	store           ports.Store
	channel         ports.ChannelService
	listService     ports.ListService
	scheduleService ports.ScheduleService
	helpText        string
}

// NewHandler constructs a Handler with dependencies.
func NewHandler(
	logger ports.Logger,
	slasher ports.SlashCommandService,
	user ports.UserService,
	store ports.Store,
	channel ports.ChannelService,
	listSvc ports.ListService,
	scheduleSvc ports.ScheduleService,
	helpText string,
) *Handler {
	logger.Debug("Creating new command Handler")
	return &Handler{
		logger:          logger,
		slasher:         slasher,
		user:            user,
		store:           store,
		channel:         channel,
		listService:     listSvc,
		scheduleService: scheduleSvc,
		helpText:        helpText,
	}
}

// Register registers the slash command.
func (h *Handler) Register() error {
	h.logger.Debug("Registering slash command")
	err := h.slasher.Register(h.scheduleDefinition())
	if err != nil {
		h.logger.Error("Failed to register slash command", "trigger", constants.CommandTrigger, "error", err)
		return err
	}
	return nil
}

// Execute handles the slash command.
func (h *Handler) Execute(args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	h.logger.Debug("Executing command", "user_id", args.UserId, "channel_id", args.ChannelId, "command", args.Command)
	commandText := strings.TrimSpace(args.Command[len("/"+constants.CommandTrigger):])

	switch {
	case strings.HasPrefix(commandText, constants.SubcommandHelp):
		h.logger.Debug("Handling help subcommand", "user_id", args.UserId)
		return h.scheduleHelp(), nil
	case strings.HasPrefix(commandText, constants.SubcommandList):
		h.logger.Debug("Handling list subcommand", "user_id", args.UserId)
		return h.BuildEphemeralList(args), nil
	case strings.HasPrefix(commandText, constants.SubcommandDelete):
		h.logger.Debug("Handling delete subcommand", "user_id", args.UserId)
		return h.handleDelete(args.UserId, strings.TrimSpace(commandText[len(constants.SubcommandDelete):])), nil
	default:
		h.logger.Debug("Handling schedule subcommand", "user_id", args.UserId, "command_text", commandText)
		return h.handleSchedule(args, commandText), nil
	}
}

// BuildEphemeralList builds the scheduled messages list response.
func (h *Handler) BuildEphemeralList(args *model.CommandArgs) *model.CommandResponse {
	h.logger.Debug("Building ephemeral list response", "user_id", args.UserId)
	return h.listService.Build(args.UserId)
}

// UserDeleteMessage validates and deletes a scheduled message.
func (h *Handler) UserDeleteMessage(userID string, msgID string) (*types.ScheduledMessage, error) {
	h.logger.Debug("Attempting to delete message", "user_id", userID, "message_id", msgID)
	msg, err := h.store.GetScheduledMessage(msgID)
	if err != nil {
		h.logger.Error("Failed to get scheduled message for deletion", "message_id", msgID, "error", err)
		return nil, err
	}
	if msg.UserID != userID {
		h.logger.Warn("User attempted to delete message owned by another user", "requesting_user_id", userID, "message_id", msgID, "owner_user_id", msg.UserID)
		return nil, fmt.Errorf("user %s attempted to delete message %s owned by %s", userID, msgID, msg.UserID)
	}
	err = h.store.DeleteScheduledMessage(userID, msgID)
	if err != nil {
		h.logger.Error("Failed to delete scheduled message from store", "user_id", userID, "message_id", msgID, "error", err)
		return nil, fmt.Errorf("failed to delete scheduled message %s: %w", msgID, err)
	}
	h.logger.Info("Successfully deleted scheduled message", "user_id", userID, "message_id", msgID)
	return msg, nil
}

// UserSendMessage validates a scheduled message for immediate send.
func (h *Handler) UserSendMessage(userID string, msgID string) (*types.ScheduledMessage, error) {
	h.logger.Debug("Attempting to send message immediately", "user_id", userID, "message_id", msgID)
	msg, err := h.store.GetScheduledMessage(msgID)
	if err != nil {
		h.logger.Error("Failed to get scheduled message for send", "message_id", msgID, "error", err)
		return nil, err
	}
	if msg.UserID != userID {
		h.logger.Warn("User attempted to send message owned by another user", "requesting_user_id", userID, "message_id", msgID, "owner_user_id", msg.UserID)
		return nil, fmt.Errorf("user %s attempted to send message %s owned by %s", userID, msgID, msg.UserID)
	}
	h.logger.Info("Successfully validated scheduled message for send", "user_id", userID, "message_id", msgID)
	return msg, nil
}

func (h *Handler) scheduleDefinition() *model.Command {
	return &model.Command{
		Trigger:          constants.CommandTrigger,
		AutoComplete:     true,
		AutoCompleteDesc: constants.AutocompleteDesc,
		AutoCompleteHint: constants.AutocompleteHint,
		AutocompleteData: h.getScheduleAutocompleteData(),
		DisplayName:      constants.CommandDisplayName,
		Description:      constants.CommandDescription,
	}
}

func (h *Handler) getScheduleAutocompleteData() *model.AutocompleteData {
	schedule := model.NewAutocompleteData(constants.CommandTrigger, constants.AutocompleteHint, constants.AutocompleteDesc)

	at := model.NewAutocompleteData(constants.SubcommandAt, constants.AutocompleteAtHint, constants.AutocompleteAtDesc)
	at.AddTextArgument(constants.AutocompleteAtArgTimeName, constants.AutocompleteAtArgTimeHint, "")
	at.AddTextArgument(constants.AutocompleteAtArgDateName, constants.AutocompleteAtArgDateHint, "")
	at.AddTextArgument(constants.AutocompleteAtArgMsgName, constants.AutocompleteAtArgMsgHint, "")
	schedule.AddCommand(at)

	del := model.NewAutocompleteData(constants.SubcommandDelete, constants.AutocompleteDeleteHint, constants.AutocompleteDeleteDesc)
	del.AddTextArgument("Number or id", constants.AutocompleteDeleteHint, "")
	schedule.AddCommand(del)

	list := model.NewAutocompleteData(constants.SubcommandList, constants.AutocompleteListHint, constants.AutocompleteListDesc)
	schedule.AddCommand(list)

	help := model.NewAutocompleteData(constants.SubcommandHelp, constants.AutocompleteHelpHint, constants.AutocompleteHelpDesc)
	schedule.AddCommand(help)

	return schedule
}

func (h *Handler) scheduleHelp() *model.CommandResponse {
	h.logger.Debug("Generating help response")
	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         h.helpText,
	}
}

func (h *Handler) handleSchedule(args *model.CommandArgs, text string) *model.CommandResponse {
	h.logger.Debug("Building schedule response", "user_id", args.UserId, "command_text", text)
	return h.scheduleService.Build(args, text)
}

func (h *Handler) handleDelete(userID string, ref string) *model.CommandResponse {
	if ref == "" || strings.ContainsAny(ref, " \t\n") {
		return errorResponse(fmt.Sprintf("%s Usage: `/%s %s <number|id>`. Run `/%s %s` to see the numbers and ids.",
			constants.EmojiError, constants.CommandTrigger, constants.SubcommandDelete, constants.CommandTrigger, constants.SubcommandList))
	}
	number, isNumber := listNumber(ref)
	if !isNumber && len(ref) < constants.ShortIDLength {
		return errorResponse(fmt.Sprintf("%s Use the list number or at least %d characters of the id, e.g. `/%s %s 1`.",
			constants.EmojiError, constants.ShortIDLength, constants.CommandTrigger, constants.SubcommandDelete))
	}

	msgs, err := loadUserMessages(h.logger, h.store, userID)
	if err != nil {
		return errorResponse(fmt.Sprintf("%s Error retrieving message list: %v", constants.EmojiError, err))
	}
	target, resolveErr := resolveDeleteTarget(msgs, ref, number, isNumber)
	if resolveErr != "" {
		return errorResponse(fmt.Sprintf("%s %s", constants.EmojiError, resolveErr))
	}

	deleted, err := h.UserDeleteMessage(userID, target.ID)
	if err != nil {
		return errorResponse(fmt.Sprintf("%s Could not delete message: %v", constants.EmojiError, err))
	}
	loc, locErr := time.LoadLocation(deleted.Timezone)
	if locErr != nil {
		loc = time.UTC
	}
	channelLink := h.channel.MakeChannelLink(h.channel.GetInfoOrUnknown(deleted.ChannelID))
	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         formatter.FormatDeleteSuccess(deleted.PostAt.In(loc), channelLink),
	}
}

// listNumber reads ref as a `/schedule list` number. Digit strings as long as a short id
// are treated as ids, since a uuid prefix can be all digits.
func listNumber(ref string) (int, bool) {
	if len(ref) >= constants.ShortIDLength || strings.Trim(ref, "0123456789") != "" {
		return 0, false
	}
	number, err := strconv.Atoi(ref)
	return number, err == nil
}

func resolveDeleteTarget(msgs []*types.ScheduledMessage, ref string, number int, isNumber bool) (*types.ScheduledMessage, string) {
	listCmd := fmt.Sprintf("/%s %s", constants.CommandTrigger, constants.SubcommandList)
	if isNumber {
		if number < 1 || number > len(msgs) {
			return nil, fmt.Sprintf("No scheduled message #%d. Run `%s` to see the numbers.", number, listCmd)
		}
		return msgs[number-1], ""
	}
	var matches []*types.ScheduledMessage
	for _, m := range msgs {
		if strings.HasPrefix(m.ID, ref) {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Sprintf("No scheduled message matches id `%s`. Run `%s` to see the ids.", ref, listCmd)
	case 1:
		return matches[0], ""
	default:
		return nil, fmt.Sprintf("Id `%s` matches %d scheduled messages. Use more of the id or the list number.", ref, len(matches))
	}
}
