/* Mattermost provides React; this file ships directly without a bundler. */
(() => {
    const pluginId = 'io.github.sn-kinos.move-to-thread';
    const React = window.React;
    const h = React.createElement;

    // One selection at a time, always inside one channel.
    let selection = {channelId: '', ids: [], targetId: '', busy: false, error: ''};
    const listeners = new Set();
    const set = changes => {
        selection = {...selection, ...changes};
        highlight.textContent = selection.ids.map(id => `#post_${id}, #rhsPost_${id}`).join(', ') +
            (selection.ids.length ? ' {background: rgba(var(--button-bg-rgb), 0.12); box-shadow: inset 3px 0 0 var(--button-bg);}' : '');
        listeners.forEach(listener => listener());
    };
    const clear = () => set({channelId: '', ids: [], targetId: '', busy: false, error: ''});
    const subscribe = listener => {
        listeners.add(listener);
        return () => listeners.delete(listener);
    };
    const useSelection = () => React.useSyncExternalStore(subscribe, () => selection);

    const highlight = document.createElement('style');
    document.head.append(highlight);

    const movable = post => post && !post.type?.startsWith('system_') && post.id !== post.pending_post_id &&
        post.state !== 'DELETED';

    const toggle = post => {
        if (selection.channelId !== post.channel_id) {
            set({channelId: post.channel_id, ids: [post.id], targetId: '', error: ''});
            return;
        }
        const ids = selection.ids.includes(post.id) ?
            selection.ids.filter(id => id !== post.id) : [...selection.ids, post.id];
        if (ids.length) set({ids, targetId: '', error: ''});
        else clear();
    };

    // While a selection exists, a plain click on a message adds or removes it.
    const clickToToggle = store => event => {
        if (!selection.ids.length || selection.busy || event.button !== 0 ||
            event.shiftKey || event.altKey || event.ctrlKey || event.metaKey) return;
        const element = event.target.closest?.('[id^="post_"], [id^="rhsPost_"]');
        if (!element || event.target.closest('a, button, input, textarea, video, img, [role="button"], [role="menuitem"], .post-menu')) return;
        if (window.getSelection?.().toString()) return;
        const post = store.getState().entities.posts.posts[element.id.slice(element.id.indexOf('_') + 1)];
        if (!movable(post)) return;
        // Otherwise Mattermost's own click handler opens the thread on the right.
        event.stopPropagation();
        toggle(post);
    };

    const move = async store => {
        set({busy: true, error: ''});
        const csrf = document.cookie.match(/(?:^|;\s*)MMCSRF=([^;]+)/)?.[1] || '';
        try {
            const response = await fetch(`/plugins/${pluginId}/move`, {
                method: 'POST', credentials: 'same-origin',
                headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrf, 'X-Requested-With': 'XMLHttpRequest'},
                body: JSON.stringify({target_id: selection.targetId, post_ids: selection.ids}),
            });
            const body = await response.json().catch(() => ({}));
            if (!response.ok) throw new Error(body.error || '메시지를 옮기지 못했습니다. 잠시 후 다시 시도해 주세요.');
            const channelId = selection.channelId;
            clear();
            store.dispatch({type: 'SELECT_POST', postId: body.root_id, channelId, timestamp: Date.now()});
        } catch (error) {
            set({busy: false, error: error.message || '메시지를 옮기지 못했습니다. 잠시 후 다시 시도해 주세요.'});
        }
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
        if (current.ids.length && !selected && current.channelId === post.channel_id) {
            buttons.push(button('선택한 메시지를 여기 답글로 넣기', 'icon-message-plus-outline',
                () => set({targetId: post.id, error: ''})));
        }
        return buttons;
    };

    const SelectionBar = ({store}) => {
        const current = useSelection();
        if (!current.ids.length) return null;
        const count = current.ids.length;
        const text = current.targetId ?
            `선택한 메시지 ${count}개를 이 메시지의 답글로 옮길까요? 원본은 지워지고 링크가 바뀝니다.` :
            `${count}개 선택됨 · 다른 메시지를 눌러 더 고르고, 넣을 메시지에서 + 말풍선 버튼을 누르세요.`;
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
        current.targetId && action(current.busy ? '옮기는 중…' : '옮기기', () => move(store), true),
        action(current.targetId ? '돌아가기' : '취소', () => (current.targetId ? set({targetId: '', error: ''}) : clear())));
    };

    window.registerPlugin(pluginId, {
        initialize(registry, store) {
            registry.registerPostActionComponent(PostAction);
            registry.registerRootComponent(() => h(SelectionBar, {store}));
            const onClick = clickToToggle(store);
            document.addEventListener('click', onClick, true);
            let channelId = store.getState().entities.channels.currentChannelId;
            const unsubscribe = store.subscribe(() => {
                const next = store.getState().entities.channels.currentChannelId;
                if (next === channelId) return;
                channelId = next;
                if (selection.ids.length && selection.channelId !== next) clear();
            });
            this.uninitialize = () => {
                document.removeEventListener('click', onClick, true);
                unsubscribe();
                clear();
            };
        },
    });
})();
