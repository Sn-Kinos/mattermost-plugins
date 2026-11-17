/* Mattermost provides React; this file ships directly without a bundler. */
(() => {
    const pluginId = 'io.github.sn-kinos.move-to-thread';
    const React = window.React;
    const h = React.createElement;

    // One selection at a time, always inside one channel. target, once
    // picked, is {kind: 'thread', postId, channelId} or {kind: 'channel', channelId}.
    let selection = {channelId: '', ids: [], target: null, busy: false, error: ''};
    const listeners = new Set();
    const set = changes => {
        selection = {...selection, ...changes};
        highlight.textContent = selection.ids.map(id => `#post_${id}, #rhsPost_${id}`).join(', ') +
            (selection.ids.length ? ' {background: rgba(var(--button-bg-rgb), 0.12); box-shadow: inset 3px 0 0 var(--button-bg);}' : '');
        listeners.forEach(listener => listener());
    };
    const clear = () => set({channelId: '', ids: [], target: null, busy: false, error: ''});
    const subscribe = listener => {
        listeners.add(listener);
        return () => listeners.delete(listener);
    };
    const useSelection = () => React.useSyncExternalStore(subscribe, () => selection);

    const highlight = document.createElement('style');
    document.head.append(highlight);

    const movable = post => post && !post.type?.startsWith('system_') && post.type !== 'burn_on_read' && post.id !== post.pending_post_id &&
        post.state !== 'DELETED';

    const toggle = post => {
        if (selection.channelId !== post.channel_id) {
            set({channelId: post.channel_id, ids: [post.id], target: null, error: ''});
            return;
        }
        const ids = selection.ids.includes(post.id) ?
            selection.ids.filter(id => id !== post.id) : [...selection.ids, post.id];
        if (ids.length) set({ids, target: null, error: ''});
        else clear();
    };

    // While a selection exists, a plain click on a message of its channel adds
    // or removes it.
    const clickToToggle = store => event => {
        if (!selection.ids.length || selection.busy || event.button !== 0 ||
            event.shiftKey || event.altKey || event.ctrlKey || event.metaKey) return;
        const element = event.target.closest?.('[id^="post_"], [id^="rhsPost_"]');
        if (!element || event.target.closest('a, button, input, textarea, video, img, [role="button"], [role="menuitem"], .post-menu')) return;
        if (window.getSelection?.().toString()) return;
        const post = store.getState().entities.posts.posts[element.id.slice(element.id.indexOf('_') + 1)];
        if (!movable(post) || post.channel_id !== selection.channelId) return;
        // Otherwise Mattermost's own click handler opens the thread on the right.
        event.stopPropagation();
        toggle(post);
    };

    const apiPost = (url, body) => fetch(url, {
        method: 'POST', credentials: 'same-origin',
        headers: {
            'Content-Type': 'application/json',
            'X-CSRF-Token': document.cookie.match(/(?:^|;\s*)MMCSRF=([^;]+)/)?.[1] || '',
            'X-Requested-With': 'XMLHttpRequest',
        },
        body: JSON.stringify(body),
    });
    const apiGet = url => fetch(url, {credentials: 'same-origin', headers: {'X-Requested-With': 'XMLHttpRequest'}});

    const move = async store => {
        const {target, ids} = selection;
        set({busy: true, error: ''});
        try {
            const destination = target.kind === 'thread' ? {target_id: target.postId} : {channel_id: target.channelId};
            const response = await apiPost(`/plugins/${pluginId}/move`, {...destination, post_ids: ids});
            const body = await response.json().catch(() => ({}));
            if (!response.ok) throw new Error(body.error || '메시지를 옮기지 못했습니다. 잠시 후 다시 시도해 주세요.');
            clear();
            if (target.kind === 'thread') {
                store.dispatch({type: 'SELECT_POST', postId: body.root_id, channelId: target.channelId, timestamp: Date.now()});
            }
        } catch (error) {
            set({busy: false, error: error.message || '메시지를 옮기지 못했습니다. 잠시 후 다시 시도해 주세요.'});
        }
    };

    const collapsedThreads = state => {
        const setting = state.entities.general.config.CollapsedThreads;
        return setting === 'always_on' || (setting !== 'disabled' &&
            (state.entities.preferences.myPreferences['display_settings--collapsed_reply_threads']?.value ||
                (setting === 'default_on' ? 'on' : 'off')) === 'on');
    };

    const removeKnown = (store, ids) => {
        const known = ids.map(id => store.getState().entities.posts.posts[id]).filter(Boolean);
        known.forEach(post => store.dispatch({type: 'POST_REMOVED', data: post}));
    };

    // Reply counts of the threads that gained or lost replies. Followed
    // threads draw their count from their own record, not the post.
    const refreshCounts = async (store, roots, sourceIds) => {
        const state = store.getState();
        const crt = collapsedThreads(state);
        roots = [...roots];
        for (const id of sourceIds || []) {
            const source = await apiGet(`/api/v4/posts/${id}`);
            if (!source.ok) continue;
            const post = await source.json();
            store.dispatch({type: 'RECEIVED_POST', data: post, features: {crtEnabled: crt}});
            roots.push(post);
        }
        const followed = store.getState().entities.threads?.threads || {};
        for (const post of roots.filter(post => post && followed[post.id])) {
            store.dispatch({
                type: 'RECEIVED_THREAD',
                data: {thread: {...followed[post.id], reply_count: post.reply_count}, team_id: state.entities.teams.currentTeamId},
            });
        }
    };

    // Puts moved posts into the channel's list of recent posts by time. Under
    // collapsed threads the list holds only roots.
    const placeInChannel = (store, channelId, moved) => {
        const state = store.getState();
        const crt = collapsedThreads(state);
        const listed = moved.filter(post => !crt || !post.root_id).sort((a, b) => b.create_at - a.create_at);
        const recent = state.entities.posts.postsInChannel?.[channelId]?.find(block => block.recent);
        if (!listed.length || !recent) return;
        const posts = Object.fromEntries(listed.map(post => [post.id, post]));
        if (recent.oldest || !recent.order.length) {
            // RECEIVED_POSTS_SINCE would drop posts older than the oldest one
            // shown, but here nothing older is left to load on scroll.
            const all = {...state.entities.posts.posts, ...posts};
            const order = [...new Set([...recent.order, ...listed.map(post => post.id)])].
                sort((a, b) => all[b].create_at - all[a].create_at);
            store.dispatch({type: 'RECEIVED_POSTS_IN_CHANNEL', channelId, data: {order, posts}, recent: true, oldest: Boolean(recent.oldest)});
            return;
        }
        store.dispatch({type: 'RECEIVED_POSTS_SINCE', channelId, data: {order: listed.map(post => post.id), posts}});
    };

    // The server moved posts in place without telling Mattermost's own client,
    // so take them out of where this screen has them and load their new place.
    const movedIntoThread = async (store, data) => {
        const state = store.getState();
        const known = data.post_ids.filter(id => state.entities.posts.posts[id]);
        if (!known.length && !state.entities.posts.posts[data.root_id]) return;
        const response = await apiGet(`/api/v4/posts/${data.root_id}/thread`);
        removeKnown(store, data.post_ids);
        if (!response.ok) return;
        const thread = await response.json();
        store.dispatch({type: 'RECEIVED_POSTS', data: thread, channelId: data.channel_id});
        store.dispatch({type: 'RECEIVED_POSTS_IN_THREAD', data: thread, rootId: data.root_id});
        await refreshCounts(store, [thread.posts[data.root_id]], data.source_roots);
        placeInChannel(store, data.channel_id, data.post_ids.map(id => thread.posts[id]).filter(Boolean));
    };

    const movedIntoChannel = async (store, data) => {
        const state = store.getState();
        const known = data.post_ids.filter(id => state.entities.posts.posts[id]);
        if (!known.length && !state.entities.posts.postsInChannel?.[data.channel_id]) return;
        removeKnown(store, data.post_ids);
        const response = await apiPost('/api/v4/posts/ids', data.post_ids);
        if (!response.ok) return;
        const moved = await response.json();
        store.dispatch({
            type: 'RECEIVED_POSTS',
            data: {order: moved.map(post => post.id), posts: Object.fromEntries(moved.map(post => [post.id, post]))},
            channelId: data.channel_id,
        });
        placeInChannel(store, data.channel_id, moved);
    };

    const moved = store => ({data}) => (data.root_id ? movedIntoThread(store, data) : movedIntoChannel(store, data));

    // Sent to the channel the posts left, which may not see where they went.
    const movedOut = store => async ({data}) => {
        removeKnown(store, data.post_ids);
        await refreshCounts(store, [], data.source_roots);
    };

    const button = (label, icon, onClick, active) => h('button', {
        key: label,
        type: 'button',
        className: 'post-menu__item move-to-thread' + (active ? ' post-menu__item--active' : ''),
        'aria-label': label,
        title: label,
        onClick,
    }, h('i', {className: 'icon icon--small ' + icon}));

    const PostAction = ({post}) => {
        const current = useSelection();
        if (!movable(post)) return null;
        const selected = current.ids.includes(post.id);
        const buttons = [selected ?
            button('선택 해제', 'icon-checkbox-marked', () => toggle(post), true) :
            button('답글로 옮길 메시지로 선택', 'icon-checkbox-blank-outline', () => toggle(post))];
        if (current.ids.length && !selected) {
            buttons.push(button('선택한 메시지를 여기 답글로 넣기', 'icon-message-plus-outline',
                () => set({target: {kind: 'thread', postId: post.id, channelId: post.channel_id}, error: ''})));
        }
        return buttons;
    };

    const SelectionBar = ({store}) => {
        const current = useSelection();
        const channelId = React.useSyncExternalStore(store.subscribe, () => store.getState().entities.channels.currentChannelId);
        if (!current.ids.length) return null;
        const count = current.ids.length;
        const away = channelId !== current.channelId;
        let text = `${count}개 선택됨 · 다른 메시지를 눌러 더 고르고, 넣을 메시지에서 + 말풍선 버튼을 누르세요.`;
        if (current.target?.kind === 'thread') text = `선택한 메시지 ${count}개를 이 메시지의 답글로 옮길까요?`;
        else if (current.target) text = `선택한 메시지 ${count}개를 이 채널로 옮길까요?`;
        else if (away) text = `${count}개 선택됨 · 넣을 메시지에서 + 말풍선 버튼을 누르거나 이 채널로 옮기세요.`;
        const action = (label, onClick, primary) => h('button', {
            type: 'button',
            className: 'btn btn-sm ' + (primary ? 'btn-primary' : 'btn-tertiary'),
            disabled: current.busy,
            onClick,
        }, label);
        return h('div', {
            className: 'move-to-thread-bar',
            role: 'status',
            style: {
                position: 'fixed', left: '50%', bottom: '88px', transform: 'translateX(-50%)', zIndex: 1000,
                display: 'flex', alignItems: 'center', gap: '12px', maxWidth: 'calc(100vw - 32px)',
                padding: '10px 12px 10px 16px', borderRadius: '8px', fontSize: '14px',
                background: 'var(--center-channel-bg)', color: 'var(--center-channel-color)',
                border: '1px solid rgba(var(--center-channel-color-rgb), 0.16)',
                boxShadow: '0 8px 24px rgba(0, 0, 0, 0.16)',
            },
        },
        h('span', null, text, current.error && h('span', {style: {display: 'block', color: 'var(--error-text)'}}, current.error)),
        current.target && action(current.busy ? '옮기는 중…' : '옮기기', () => move(store), true),
        !current.target && away && action('이 채널로 옮기기', () => set({target: {kind: 'channel', channelId}, error: ''}), true),
        action(current.target ? '돌아가기' : '취소', () => (current.target ? set({target: null, error: ''}) : clear())));
    };

    window.registerPlugin(pluginId, {
        initialize(registry, store) {
            registry.registerPostActionComponent(PostAction);
            registry.registerRootComponent(() => h(SelectionBar, {store}));
            registry.registerWebSocketEventHandler(`custom_${pluginId}_moved`, moved(store));
            registry.registerWebSocketEventHandler(`custom_${pluginId}_moved_out`, movedOut(store));
            const onClick = clickToToggle(store);
            document.addEventListener('click', onClick, true);
            let channelId = store.getState().entities.channels.currentChannelId;
            const unsubscribe = store.subscribe(() => {
                const next = store.getState().entities.channels.currentChannelId;
                if (next === channelId) return;
                channelId = next;
                // "이 채널" and "이 메시지" in a pending confirmation no longer
                // name what is on screen.
                if (selection.target && !selection.busy) set({target: null, error: ''});
            });
            this.uninitialize = () => {
                document.removeEventListener('click', onClick, true);
                unsubscribe();
                clear();
            };
        },
    });
})();
