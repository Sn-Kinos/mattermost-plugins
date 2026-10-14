/* Mattermost provides React; this file ships directly without a bundler. */
(() => {
    const React = window.React;
    const h = React.createElement;
    const locations = ['CENTER', 'RHS_ROOT', 'RHS_COMMENT', 'SEARCH'];
    const moreButtons = locations.map(location => `[id^="${location}_button_"]`);

    // While Shift is held the original toolbar items collapse in place, so More and the reaction
    // button still anchor the menu and emoji picker the plugin buttons open.
    const style = document.createElement('style');
    style.textContent = '.post-menu:has(.shift-actions) > li:not(:has(.shift-actions)) ' +
        '{width: 0; min-width: 0; padding: 0; margin: 0; border: 0; overflow: hidden;}\n' +
        'body.shift-actions-proxy .MuiPopover-root:has([id*="_dropdown_"]), ' +
        'body.shift-actions-proxy .modal:has(#deletePostModalButton), body.shift-actions-proxy .modal-backdrop {opacity: 0;}';
    document.head.append(style);

    let shift = false;
    const listeners = new Set();
    const track = event => {
        const held = event.type !== 'blur' && event.shiftKey;
        if (held === shift) return;
        shift = held;
        listeners.forEach(listener => listener());
    };
    for (const type of ['keydown', 'keyup', 'mousemove', 'blur']) window.addEventListener(type, track);
    const subscribeShift = listener => {
        listeners.add(listener);
        return () => listeners.delete(listener);
    };

    const find = async id => {
        for (let waited = 0; waited < 300; waited += 20) {
            const element = document.getElementById(id);
            if (element) return element;
            await new Promise(resolve => setTimeout(resolve, 20));
        }
        return null;
    };

    // Opens the post's own More menu and activates one of its items, so Mattermost keeps
    // handling permissions, toasts, edit mode and modals. The menu stays invisible unless it
    // opens a submenu; delete also confirms its dialog invisibly, like Discord's Shift-delete.
    const menuItem = async (button, itemId, mode) => {
        const visible = mode === 'submenu';
        const more = button.closest('.post-menu').querySelector(moreButtons.join(', '));
        if (!visible) document.body.classList.add('shift-actions-proxy');
        try {
            more.click();
            // Closing the menu returns focus to More, which would pin this post's toolbar open.
            more.addEventListener('focus', () => more.blur(), {once: true});
            const item = await find(itemId);
            if (!item) {
                (document.activeElement || document).dispatchEvent(new KeyboardEvent('keydown', {key: 'Escape', bubbles: true}));
            } else if (visible) {
                item.focus();
                item.dispatchEvent(new KeyboardEvent('keydown', {key: 'ArrowRight', bubbles: true}));
            } else {
                item.click();
                if (mode === 'confirm') (await find('deletePostModalButton'))?.click();
            }
        } finally {
            if (!visible) setTimeout(() => document.body.classList.remove('shift-actions-proxy'), 200);
        }
    };

    const actions = (store) => function ShiftActions({post}) {
        const held = React.useSyncExternalStore(subscribeShift, () => shift);
        const intl = window.ReactIntl.useIntl();
        if (!held) return null;

        const {users, general, preferences, threads} = store.getState().entities;
        const me = users.currentUserId;
        const mayChange = post.user_id === me || (users.profiles[me]?.roles || '').split(' ').includes('system_admin');
        const crtSetting = general.config.CollapsedThreads;
        const crt = crtSetting === 'always_on' || (crtSetting !== 'disabled' &&
            (preferences.myPreferences['display_settings--collapsed_reply_threads']?.value ||
                (crtSetting === 'default_on' ? 'on' : 'off')) === 'on');
        const rootId = post.root_id || post.id;
        const following = threads.threads[rootId]?.is_following;
        const thread = Boolean(post.root_id || post.reply_count);
        const follow = 'threading.threadMenu.' + (following ? 'unfollow' : 'follow') + (thread ? '' : 'Message');
        const saved = preferences.myPreferences['flagged_post--' + post.id]?.value === 'true';

        const buttons = [
            ['post_info.reply', 'Reply', 'icon-reply-outline', 'reply_to_post_'],
            ['forward_post_button.label', 'Forward', 'icon-arrow-right-bold-outline', 'forward_post_'],
            ['rhs_root.mobile.add_reaction', 'Add Reaction', 'icon-emoticon-plus-outline', '_reaction_', 'toolbar'],
            crt && [follow, 'Follow', following ? 'icon-message-minus-outline' : 'icon-message-check-outline', 'follow_post_thread_'],
            ['post_info.unread', 'Mark as Unread', 'icon-mark-as-unread', 'unread_post_'],
            saved ?
                ['rhs_root.mobile.unflag', 'Remove from Saved', 'icon-bookmark', 'save_post_'] :
                ['rhs_root.mobile.flag', 'Save Message', 'icon-bookmark-outline', 'save_post_'],
            ['post_info.post_reminder.menu', 'Remind', 'icon-clock-outline', 'remind_post_', 'submenu'],
            post.is_pinned ?
                ['post_info.unpin', 'Unpin from Channel', 'icon-pin', 'unpin_post_'] :
                ['post_info.pin', 'Pin to Channel', 'icon-pin-outline', 'pin_post_'],
            ['post_info.copy', 'Copy Text', 'icon-content-copy', 'copy_'],
            ['post_info.permalink', 'Copy Link', 'icon-link-variant', 'permalink_'],
            mayChange && ['post_info.edit', 'Edit', 'icon-pencil-outline', 'edit_post_'],
            mayChange && ['post_info.del', 'Delete', 'icon-trash-can-outline', 'delete_post_', 'confirm'],
        ].filter(Boolean);

        return buttons.map(([id, defaultMessage, icon, item, mode]) => {
            const label = intl.formatMessage({id, defaultMessage});
            return h('button', {
                key: id,
                type: 'button',
                className: 'post-menu__item shift-actions',
                'aria-label': label,
                title: label,
                style: mode === 'confirm' ? {color: 'var(--error-text)'} : undefined,
                // The desktop More menu has no Add Reaction item, so that one clicks the collapsed toolbar button.
                onClick: event => (mode === 'toolbar' ?
                    event.currentTarget.closest('.post-menu').querySelector(`[id$="${item + post.id}"]`).click() :
                    menuItem(event.currentTarget, item + post.id, mode)),
            }, h('i', {className: 'icon icon--small ' + icon}));
        });
    };

    window.registerPlugin('io.github.sn-kinos.shift-actions', {
        initialize(registry, store) {
            registry.registerPostActionComponent(actions(store));
        },
    });
})();
