const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const {JSDOM} = require('jsdom');

const posts = {
    p1: {id: 'p1', channel_id: 'c1', user_id: 'me', message: 'first'},
    p2: {id: 'p2', channel_id: 'c1', user_id: 'alex', message: 'second'},
    p3: {id: 'p3', channel_id: 'c1', user_id: 'alex', message: 'topic'},
    joined: {id: 'joined', channel_id: 'c1', user_id: 'alex', type: 'system_join_channel'},
    far: {id: 'far', channel_id: 'c2', user_id: 'me', message: 'elsewhere'},
};

function load({respond = () => ({ok: true, body: {root_id: 'p3'}}), crt = 'on'} = {}) {
    const dom = new JSDOM(`<div id="root"></div><div id="bar"></div>
        ${Object.keys(posts).map(id => `<div id="post_${id}"><p class="text">${id}</p><a href="#">link</a></div>`).join('')}`,
    {runScripts: 'outside-only', url: 'https://chat.example/team-a/channels/town'});
    const {window} = dom;
    global.window = window;
    global.document = window.document;
    global.IS_REACT_ACT_ENVIRONMENT = true;
    window.React = require('react');
    window.document.cookie = 'MMCSRF=token123';
    const state = {entities: {
        posts: {posts},
        channels: {currentChannelId: 'c1'},
        teams: {currentTeamId: 't1'},
        threads: {threads: {p3: {id: 'p3', reply_count: 0, is_following: true}, s1: {id: 's1', reply_count: 2, is_following: true}}},
        general: {config: {CollapsedThreads: 'default_on'}},
        preferences: {myPreferences: {'display_settings--collapsed_reply_threads': {value: crt}}},
    }};
    const subscribers = new Set();
    const dispatched = [];
    const requests = [];
    window.fetch = async (url, options) => {
        requests.push({url, headers: options.headers, body: options.body && JSON.parse(options.body)});
        const {ok, body} = respond(url);
        return {ok, json: async () => body};
    };
    let PostAction;
    let Root;
    const sockets = {};
    const plugin = {};
    window.registerPlugin = (id, definition) => {
        Object.assign(plugin, definition);
        plugin.initialize({
            registerPostActionComponent(component) { PostAction = component; },
            registerRootComponent(component) { Root = component; },
            registerWebSocketEventHandler(event, handler) { sockets[event] = handler; },
        }, {
            getState: () => state,
            subscribe: listener => { subscribers.add(listener); return () => subscribers.delete(listener); },
            dispatch: action => dispatched.push(action),
        });
    };
    window.eval(fs.readFileSync(`${__dirname}/plugin.js`, 'utf8'));
    const {createRoot} = require('react-dom/client');
    const {act} = require('react-dom/test-utils');
    const toolbar = createRoot(window.document.getElementById('root'));
    const bar = createRoot(window.document.getElementById('bar'));
    const hover = id => act(() => toolbar.render(window.React.createElement(PostAction, {post: posts[id]})));
    act(() => bar.render(window.React.createElement(Root)));
    const buttons = () => [...window.document.querySelectorAll('#root button')].map(b => b.getAttribute('aria-label'));
    const press = label => act(() => window.document.querySelector(`button[aria-label="${label}"]`).click());
    const barText = () => window.document.getElementById('bar').textContent;
    const barButton = label => act(async () => {
        [...window.document.querySelectorAll('#bar button')].find(b => b.textContent === label).click();
        await new Promise(resolve => setTimeout(resolve, 0));
    });
    const clickPost = (id, selector = '.text') => act(() => {
        window.document.querySelector(`#post_${id} ${selector}`).dispatchEvent(new window.MouseEvent('click', {bubbles: true, button: 0}));
    });
    // Mattermost's own post click handler, which opens the thread on the right.
    const postClicks = [];
    for (const id of Object.keys(posts)) {
        window.document.getElementById('post_' + id).addEventListener('click', () => postClicks.push(id));
    }
    const highlighted = () => [...window.document.querySelectorAll('style')].map(s => s.textContent).join('');
    const switchChannel = id => act(() => {
        state.entities.channels.currentChannelId = id;
        subscribers.forEach(listener => listener());
    });
    const socket = (event, data) => act(async () => {
        sockets[event]({event, data, broadcast: {channel_id: 'c1'}});
        await new Promise(resolve => setTimeout(resolve, 0));
    });
    return {hover, buttons, press, barText, barButton, clickPost, postClicks, highlighted, switchChannel, requests, dispatched, plugin, socket};
}

test('select a message from its toolbar, add more by clicking, then move them under a target', async () => {
    const t = load();
    await t.hover('p1');
    assert.deepEqual(t.buttons(), ['답글로 옮길 메시지로 선택']);
    assert.equal(t.barText(), '');

    await t.press('답글로 옮길 메시지로 선택');
    assert.deepEqual(t.buttons(), ['선택 해제']);
    assert.match(t.barText(), /^1개 선택됨/);
    assert.match(t.highlighted(), /#post_p1, #rhsPost_p1 \{background/);

    await t.clickPost('p2');
    await t.clickPost('p2', 'a');
    assert.match(t.barText(), /^2개 선택됨/);
    assert.deepEqual(t.postClicks, ['p2'], 'a selecting click does not reach Mattermost; a link click does');

    await t.hover('p3');
    assert.deepEqual(t.buttons(), ['답글로 옮길 메시지로 선택', '선택한 메시지를 여기 답글로 넣기']);
    await t.press('선택한 메시지를 여기 답글로 넣기');
    assert.equal(t.barText(), '선택한 메시지 2개를 이 메시지의 답글로 옮길까요?옮기기돌아가기');

    await t.barButton('옮기기');
    assert.deepEqual(JSON.parse(JSON.stringify(t.requests)), [{
        url: '/plugins/io.github.sn-kinos.move-to-thread/move',
        headers: {'Content-Type': 'application/json', 'X-CSRF-Token': 'token123', 'X-Requested-With': 'XMLHttpRequest'},
        body: {target_id: 'p3', post_ids: ['p1', 'p2']},
    }]);
    assert.equal(t.barText(), '');
    assert.doesNotMatch(t.highlighted(), /#post_/);
    assert.equal(t.dispatched[0].type, 'SELECT_POST');
    assert.equal(t.dispatched[0].postId, 'p3');
    assert.equal(t.dispatched[0].channelId, 'c1');
});

test('a failed move keeps the selection and shows the server message', async () => {
    const t = load({respond: () => ({ok: false, body: {error: '지울 권한이 없는 메시지는 옮길 수 없습니다.'}})});
    await t.hover('p1');
    await t.press('답글로 옮길 메시지로 선택');
    await t.hover('p3');
    await t.press('선택한 메시지를 여기 답글로 넣기');
    await t.barButton('옮기기');
    assert.match(t.barText(), /지울 권한이 없는 메시지는 옮길 수 없습니다\./);
    await t.barButton('돌아가기');
    assert.match(t.barText(), /^1개 선택됨/);
});

test('system messages get no buttons and clicks on them do not select', async () => {
    const t = load();
    await t.hover('joined');
    assert.deepEqual(t.buttons(), []);
    await t.hover('p1');
    await t.press('답글로 옮길 메시지로 선택');
    await t.clickPost('joined');
    assert.match(t.barText(), /^1개 선택됨/);
});

test('selecting in another channel starts over, and switching channels clears the selection', async () => {
    const t = load();
    await t.hover('p1');
    await t.press('답글로 옮길 메시지로 선택');
    await t.hover('far');
    assert.deepEqual(t.buttons(), ['답글로 옮길 메시지로 선택']);
    await t.press('답글로 옮길 메시지로 선택');
    assert.match(t.barText(), /^1개 선택됨/);
    assert.match(t.highlighted(), /#post_far/);
    assert.doesNotMatch(t.highlighted(), /#post_p1/);

    await t.switchChannel('c3');
    assert.equal(t.barText(), '');
});

test('unselecting the last message or pressing cancel ends selection', async () => {
    const t = load();
    await t.hover('p1');
    await t.press('답글로 옮길 메시지로 선택');
    await t.press('선택 해제');
    assert.equal(t.barText(), '');
    await t.press('답글로 옮길 메시지로 선택');
    await t.barButton('취소');
    assert.equal(t.barText(), '');
    await t.clickPost('p2');
    assert.equal(t.barText(), '');
});

// The thread as the server returns it after p1 and p2 moved under p3.
const movedThread = {
    order: ['p2', 'p1', 'p3'],
    posts: {
        p3: {...posts.p3, reply_count: 2},
        p1: {...posts.p1, root_id: 'p3'},
        p2: {...posts.p2, root_id: 'p3'},
    },
};

const types = dispatched => dispatched.map(action => action.type);

const source = {id: 's1', channel_id: 'c1', user_id: 'alex', message: 'other topic', reply_count: 1};
const byUrl = url => ({ok: true, body: url.endsWith('/thread') ? movedThread : source});

test('a move announced by the server puts the posts in their new thread without a reload', async () => {
    const t = load({respond: byUrl});
    await t.socket('custom_io.github.sn-kinos.move-to-thread_moved', {root_id: 'p3', post_ids: ['p1', 'p2'], source_roots: ['s1']});

    assert.deepEqual(t.requests.map(r => r.url), ['/api/v4/posts/p3/thread', '/api/v4/posts/s1']);
    assert.deepEqual(types(t.dispatched), ['POST_REMOVED', 'POST_REMOVED', 'RECEIVED_POSTS', 'RECEIVED_POSTS_IN_THREAD',
        'RECEIVED_POST', 'RECEIVED_THREAD', 'RECEIVED_THREAD']);
    assert.deepEqual(t.dispatched.slice(0, 2).map(action => action.data.id), ['p1', 'p2']);
    assert.equal(t.dispatched[0].data.root_id, undefined, 'removes the post from where it was');
    assert.equal(t.dispatched[2].data.posts.p1.root_id, 'p3');
    assert.equal(t.dispatched[3].rootId, 'p3');
    assert.equal(t.dispatched[4].data.reply_count, 1, 'the thread the reply left shows one reply fewer');
    assert.deepEqual(t.dispatched.slice(5).map(a => [a.data.thread.id, a.data.thread.reply_count, a.data.team_id]),
        [['p3', 2, 't1'], ['s1', 1, 't1']], 'followed threads show the new reply counts');
});

test('with threads not collapsed, the moved replies also take their place in the channel by time', async () => {
    const t = load({respond: byUrl, crt: 'off'});
    await t.socket('custom_io.github.sn-kinos.move-to-thread_moved', {root_id: 'p3', post_ids: ['p1', 'p2'], source_roots: []});

    assert.deepEqual(types(t.dispatched), ['POST_REMOVED', 'POST_REMOVED', 'RECEIVED_POSTS', 'RECEIVED_POSTS_IN_THREAD',
        'RECEIVED_THREAD', 'RECEIVED_POSTS_SINCE']);
    const since = t.dispatched[5];
    assert.equal(since.channelId, 'c1');
    assert.deepEqual(since.data.order, ['p1', 'p2']);
    assert.deepEqual(Object.keys(since.data.posts), ['p1', 'p2']);
});

test('a move in a channel this screen never loaded is ignored', async () => {
    const t = load({respond: () => ({ok: true, body: movedThread})});
    await t.socket('custom_io.github.sn-kinos.move-to-thread_moved', {root_id: 'unknown', post_ids: ['nope'], source_roots: []});
    assert.deepEqual(t.requests, []);
    assert.deepEqual(t.dispatched, []);
});
