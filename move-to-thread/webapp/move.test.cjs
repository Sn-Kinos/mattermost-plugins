const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const {JSDOM} = require('jsdom');

const posts = {
    p1: {id: 'p1', channel_id: 'c1', user_id: 'me', message: 'first', create_at: 100},
    p2: {id: 'p2', channel_id: 'c1', user_id: 'alex', message: 'second', create_at: 200},
    p3: {id: 'p3', channel_id: 'c1', user_id: 'alex', message: 'topic', create_at: 50},
    joined: {id: 'joined', channel_id: 'c1', user_id: 'alex', type: 'system_join_channel', create_at: 150},
    far: {id: 'far', channel_id: 'c2', user_id: 'me', message: 'elsewhere', create_at: 300},
    r1: {id: 'r1', channel_id: 'c1', user_id: 'alex', root_id: 'p3', message: 'answer', create_at: 60},
    r2: {id: 'r2', channel_id: 'c1', user_id: 'me', root_id: 'p3', message: 'follow-up', create_at: 70},
    r3: {id: 'r3', channel_id: 'c1', user_id: 'alex', root_id: 'p3', message: 'last word', create_at: 80},
};

const loadedChannels = {
    c1: [{order: ['p2', 'joined', 'p1', 'p3'], recent: true}],
    c2: [{order: ['far'], recent: true}],
};

function load({respond = () => ({ok: true, body: {root_id: 'p3'}}), crt = 'on', postsInChannel = loadedChannels, postsInThread = {}, known = posts} = {}) {
    const dom = new JSDOM(`<div id="root"></div><div id="bar"></div>
        ${Object.keys(posts).map(id => `<div id="post_${id}"><p class="text">${id}</p><a href="#">link</a></div>`).join('')}
        ${Object.keys(posts).map(id => `<div id="rhsPost_${id}"><p class="text">${id}</p></div>`).join('')}`,
    {runScripts: 'outside-only', url: 'https://chat.example/team-a/channels/town'});
    const {window} = dom;
    global.window = window;
    global.document = window.document;
    global.IS_REACT_ACT_ENVIRONMENT = true;
    window.React = require('react');
    window.document.cookie = 'MMCSRF=token123';
    const state = {entities: {
        posts: {posts: known, postsInChannel, postsInThread},
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
        requests.push({url, method: options.method || 'GET', headers: options.headers, body: options.body && JSON.parse(options.body)});
        const {ok, body} = respond(url, options);
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
    const barButtons = () => [...window.document.querySelectorAll('#bar button')].map(b => b.textContent);
    const barButton = label => act(async () => {
        [...window.document.querySelectorAll('#bar button')].find(b => b.textContent === label).click();
        await new Promise(resolve => setTimeout(resolve, 0));
    });
    const clickPost = (id, selector = '.text', {shiftKey = false, prefix = 'post'} = {}) => act(() => {
        const target = window.document.querySelector(`#${prefix}_${id} ${selector}`);
        const down = new window.MouseEvent('mousedown', {bubbles: true, cancelable: true, button: 0, shiftKey});
        target.dispatchEvent(down);
        mouseDownsKept.push(!down.defaultPrevented);
        target.dispatchEvent(new window.MouseEvent('click', {bubbles: true, button: 0, shiftKey}));
    });
    const mouseDownsKept = [];
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
    const socket = (event, data, channel = 'c1') => act(async () => {
        sockets[`custom_io.github.sn-kinos.move-to-thread_${event}`]({event, data, broadcast: {channel_id: channel}});
        await new Promise(resolve => setTimeout(resolve, 0));
    });
    const selected = () => [...highlighted().matchAll(/#post_(\w+),/g)].map(match => match[1]);
    const bodyClass = () => window.document.body.className;
    return {hover, buttons, press, barText, barButtons, barButton, clickPost, postClicks, highlighted, selected, mouseDownsKept, bodyClass,
        switchChannel, requests, dispatched, plugin, socket};
}

test('select a message from its toolbar, add more by clicking, then move them under a target', async () => {
    const t = load();
    await t.hover('p1');
    assert.deepEqual(t.buttons(), ['답글로 옮길 메시지로 선택']);
    assert.equal(t.barText(), '');

    assert.equal(t.bodyClass(), '');
    await t.press('답글로 옮길 메시지로 선택');
    assert.deepEqual(t.buttons(), ['선택 해제']);
    assert.equal(t.bodyClass(), 'move-to-thread-selecting', 'Shift Actions stands aside while picking');
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
        method: 'POST',
        headers: {'Content-Type': 'application/json', 'X-CSRF-Token': 'token123', 'X-Requested-With': 'XMLHttpRequest'},
        body: {target_id: 'p3', post_ids: ['p1', 'p2']},
    }]);
    assert.equal(t.barText(), '');
    assert.doesNotMatch(t.highlighted(), /#post_/);
    assert.equal(t.bodyClass(), '');
    assert.equal(t.dispatched[0].type, 'SELECT_POST');
    assert.equal(t.dispatched[0].postId, 'p3');
    assert.equal(t.dispatched[0].channelId, 'c1');
});

test('shift-click adds every message from the last one picked, skipping system messages', async () => {
    const t = load();
    await t.hover('p3');
    await t.press('답글로 옮길 메시지로 선택');
    await t.clickPost('p2', '.text', {shiftKey: true});
    assert.deepEqual(t.selected(), ['p3', 'p2', 'p1'], 'p3 to p2 in the channel list, without the join message');
    assert.deepEqual(t.postClicks, [], 'the shift-click does not open the thread');
    assert.deepEqual(t.mouseDownsKept, [false], 'the shift press does not stretch the text selection');

    await t.clickPost('p1');
    assert.deepEqual(t.selected(), ['p3', 'p2']);
    await t.clickPost('p3', '.text', {shiftKey: true});
    assert.deepEqual(t.selected(), ['p3', 'p2', 'p1'], 'the range starts at the message last clicked');
});

test('shift-click in the thread on the right ranges over the thread in time order', async () => {
    const t = load({postsInThread: {p3: ['r3', 'r2', 'r1']}});
    await t.hover('r1');
    await t.press('답글로 옮길 메시지로 선택');
    await t.clickPost('r3', '.text', {shiftKey: true, prefix: 'rhsPost'});
    assert.deepEqual(t.selected(), ['r1', 'r2', 'r3']);
});

test('shift-click with the last pick in another list just toggles', async () => {
    const t = load({postsInThread: {p3: ['r1']}});
    await t.hover('p2');
    await t.press('답글로 옮길 메시지로 선택');
    await t.clickPost('r1', '.text', {shiftKey: true, prefix: 'rhsPost'});
    assert.deepEqual(t.selected(), ['p2', 'r1']);
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

test('the selection survives a channel switch; there clicks act as usual and the checkbox starts over', async () => {
    const t = load();
    await t.hover('p1');
    await t.press('답글로 옮길 메시지로 선택');
    await t.switchChannel('c2');
    assert.match(t.barText(), /^1개 선택됨/);
    assert.match(t.highlighted(), /#post_p1/);

    await t.clickPost('far');
    assert.match(t.barText(), /^1개 선택됨/);
    assert.deepEqual(t.postClicks, ['far'], 'the click reaches Mattermost');

    await t.hover('far');
    assert.deepEqual(t.buttons(), ['답글로 옮길 메시지로 선택', '선택한 메시지를 여기 답글로 넣기']);
    await t.press('답글로 옮길 메시지로 선택');
    assert.match(t.barText(), /^1개 선택됨/);
    assert.match(t.highlighted(), /#post_far/);
    assert.doesNotMatch(t.highlighted(), /#post_p1/);
    assert.deepEqual(t.barButtons(), ['취소'], 'the selection is in this channel now');
});

test('in another channel the bar moves the selection there', async () => {
    const t = load({respond: () => ({ok: true, body: {channel_id: 'c2'}})});
    await t.hover('p1');
    await t.press('답글로 옮길 메시지로 선택');
    assert.deepEqual(t.barButtons(), ['취소']);

    await t.switchChannel('c2');
    assert.deepEqual(t.barButtons(), ['이 채널로 옮기기', '취소']);
    await t.barButton('이 채널로 옮기기');
    assert.equal(t.barText(), '선택한 메시지 1개를 이 채널로 옮길까요?옮기기돌아가기');

    await t.barButton('옮기기');
    assert.deepEqual(JSON.parse(JSON.stringify(t.requests)), [{
        url: '/plugins/io.github.sn-kinos.move-to-thread/move',
        method: 'POST',
        headers: {'Content-Type': 'application/json', 'X-CSRF-Token': 'token123', 'X-Requested-With': 'XMLHttpRequest'},
        body: {channel_id: 'c2', post_ids: ['p1']},
    }]);
    assert.equal(t.barText(), '');
    assert.deepEqual(t.dispatched, [], 'the mover is already looking at the channel');
});

test('selected replies can be taken out of their thread into their own channel', async () => {
    const t = load({respond: () => ({ok: true, body: {channel_id: 'c1'}})});
    await t.hover('r1');
    await t.press('답글로 옮길 메시지로 선택');
    assert.deepEqual(t.barButtons(), ['글타래 밖으로 꺼내기', '취소']);
    await t.clickPost('p1');
    assert.deepEqual(t.barButtons(), ['취소'], 'a root is already in the channel');
    await t.clickPost('p1');

    await t.barButton('글타래 밖으로 꺼내기');
    assert.equal(t.barText(), '선택한 답글 1개를 글타래 밖으로 꺼낼까요?옮기기돌아가기');
    await t.barButton('옮기기');
    assert.deepEqual(plain(t.requests.map(r => r.body)), [{channel_id: 'c1', post_ids: ['r1']}]);
    assert.equal(t.barText(), '');
});

test('from another channel the bar offers both moving here and taking the replies out', async () => {
    const t = load();
    await t.hover('r1');
    await t.press('답글로 옮길 메시지로 선택');
    await t.switchChannel('c2');
    assert.deepEqual(t.barButtons(), ['이 채널로 옮기기', '글타래 밖으로 꺼내기', '취소']);
    await t.barButton('글타래 밖으로 꺼내기');
    assert.equal(t.barText(), '선택한 답글 1개를 글타래 밖으로 꺼낼까요?옮기기돌아가기');
});

test('leaving the channel while confirming a channel move goes back to the selection', async () => {
    const t = load();
    await t.hover('p1');
    await t.press('답글로 옮길 메시지로 선택');
    await t.switchChannel('c2');
    await t.barButton('이 채널로 옮기기');
    await t.switchChannel('c3');
    assert.match(t.barText(), /^1개 선택됨/);
    assert.deepEqual(t.barButtons(), ['이 채널로 옮기기', '취소']);
    await t.switchChannel('');
    assert.deepEqual(t.barButtons(), ['취소'], 'screens without a channel, like Threads, offer no channel to move to');
});

test('the + button on a message in another channel moves the selection into its thread', async () => {
    const t = load({respond: () => ({ok: true, body: {root_id: 'far'}})});
    await t.hover('p1');
    await t.press('답글로 옮길 메시지로 선택');
    await t.switchChannel('c2');
    await t.hover('far');
    await t.press('선택한 메시지를 여기 답글로 넣기');
    assert.equal(t.barText(), '선택한 메시지 1개를 이 메시지의 답글로 옮길까요?옮기기돌아가기');

    await t.barButton('옮기기');
    assert.deepEqual(t.requests.map(r => r.body), [{target_id: 'far', post_ids: ['p1']}]);
    assert.equal(t.dispatched[0].type, 'SELECT_POST');
    assert.equal(t.dispatched[0].postId, 'far');
    assert.equal(t.dispatched[0].channelId, 'c2');
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
        p1: {...posts.p1, root_id: 'p3', create_at: 51},
        p2: {...posts.p2, root_id: 'p3', create_at: 52},
    },
};

const types = dispatched => dispatched.map(action => action.type);

const source = {id: 's1', channel_id: 'c1', user_id: 'alex', message: 'other topic', reply_count: 1};
const byUrl = url => ({ok: true, body: url.endsWith('/thread') ? movedThread : source});

test('a move announced by the server puts the posts in their new thread without a reload', async () => {
    const t = load({respond: byUrl});
    await t.socket('moved', {root_id: 'p3', channel_id: 'c1', post_ids: ['p1', 'p2'], source_roots: ['s1']});

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
    const t = load({respond: byUrl, crt: 'off', postsInChannel: {c1: [{order: ['p2', 'joined', 'p1', 'p3'], recent: true, oldest: true}]}});
    await t.socket('moved', {root_id: 'p3', channel_id: 'c1', post_ids: ['p1', 'p2'], source_roots: []});

    assert.deepEqual(types(t.dispatched), ['POST_REMOVED', 'POST_REMOVED', 'RECEIVED_POSTS', 'RECEIVED_POSTS_IN_THREAD',
        'RECEIVED_THREAD', 'RECEIVED_POSTS_SINCE']);
    const since = t.dispatched[5];
    assert.equal(since.channelId, 'c1');
    assert.deepEqual(since.data.order, ['p1', 'p2']);
    assert.deepEqual(Object.keys(since.data.posts), ['p1', 'p2']);
});

test('a move in a channel this screen never loaded is ignored', async () => {
    const t = load({respond: () => ({ok: true, body: movedThread})});
    await t.socket('moved', {root_id: 'unknown', channel_id: 'c9', post_ids: ['nope'], source_roots: []}, 'c9');
    await t.socket('moved', {root_id: '', channel_id: 'c9', post_ids: ['nope'], source_roots: []}, 'c9');
    assert.deepEqual(t.requests, []);
    assert.deepEqual(t.dispatched, []);
});

test('posts the screen can no longer read still leave where they were', async () => {
    const t = load({respond: () => ({ok: false, body: {}})});
    await t.socket('moved', {root_id: 'hidden', channel_id: 'c9', post_ids: ['p1', 'p2'], source_roots: []}, 'c9');
    assert.deepEqual(t.requests.map(r => r.url), ['/api/v4/posts/hidden/thread']);
    assert.deepEqual(t.dispatched.map(a => [a.type, a.data.id]), [['POST_REMOVED', 'p1'], ['POST_REMOVED', 'p2']]);
});

test('the channel posts left hears only what left and updates the reply counts', async () => {
    const t = load({respond: () => ({ok: true, body: source})});
    await t.socket('moved_out', {post_ids: ['p1', 'p2', 'gone'], source_roots: ['s1']});

    assert.deepEqual(t.requests.map(r => r.url), ['/api/v4/posts/s1']);
    assert.deepEqual(t.dispatched.map(a => [a.type, a.data.id || a.data.thread.id]),
        [['POST_REMOVED', 'p1'], ['POST_REMOVED', 'p2'], ['RECEIVED_POST', 's1'], ['RECEIVED_THREAD', 's1']]);
    assert.equal(t.dispatched[3].data.thread.reply_count, 1);
});

// p1 moved to c2 as a root with its reply p2, keeping their times.
const arrivedAt = (root, reply) => (url, options) => ({
    ok: true,
    body: url === '/api/v4/posts/ids' && options.method === 'POST' ? [
        {...posts.p1, channel_id: 'c2', create_at: root},
        {...posts.p2, channel_id: 'c2', root_id: 'p1', create_at: reply},
    ] : {},
});
const byIds = arrivedAt(400, 500);
const plain = value => JSON.parse(JSON.stringify(value));

test('posts moved into a channel take their place there by time', async () => {
    for (const [crt, order] of [['on', ['p1']], ['off', ['p2', 'p1']]]) {
        const t = load({respond: byIds, crt});
        await t.socket('moved', {root_id: '', channel_id: 'c2', post_ids: ['p1', 'p2'], source_roots: ['s1']}, 'c2');

        assert.deepEqual(JSON.parse(JSON.stringify(t.requests)), [{
            url: '/api/v4/posts/ids',
            method: 'POST',
            headers: {'Content-Type': 'application/json', 'X-CSRF-Token': 'token123', 'X-Requested-With': 'XMLHttpRequest'},
            body: ['p1', 'p2'],
        }]);
        assert.deepEqual(types(t.dispatched), ['POST_REMOVED', 'POST_REMOVED', 'RECEIVED_POSTS', 'RECEIVED_POSTS_SINCE']);
        assert.equal(t.dispatched[2].channelId, 'c2');
        assert.deepEqual(t.dispatched[2].data.posts.p2.root_id, 'p1');
        const since = t.dispatched[3];
        assert.equal(since.channelId, 'c2');
        assert.deepEqual(since.data.order, order, `collapsed threads ${crt}: only roots join the channel list`);
        assert.deepEqual(Object.keys(since.data.posts), order);
    }
});

test('replies taken out of their thread show up in the channel and the thread shows fewer replies', async () => {
    const respond = (url, options) => ({
        ok: true,
        body: options.method === 'POST' ? [{...posts.r1, root_id: ''}] : {...posts.p3, reply_count: 0},
    });
    const t = load({respond});
    await t.socket('moved', {root_id: '', channel_id: 'c1', post_ids: ['r1'], source_roots: ['p3']});

    assert.deepEqual(t.requests.map(r => r.url), ['/api/v4/posts/ids', '/api/v4/posts/p3']);
    assert.deepEqual(types(t.dispatched), ['POST_REMOVED', 'RECEIVED_POSTS', 'RECEIVED_POSTS_SINCE', 'RECEIVED_POST', 'RECEIVED_THREAD']);
    assert.equal(t.dispatched[0].data.root_id, 'p3', 'removes the reply from the thread it was in');
    assert.deepEqual(t.dispatched[2].data.order, ['r1']);
    assert.equal(t.dispatched[4].data.thread.reply_count, 0);
});

test('a reply this screen already heard is a root still leaves the thread that lists it', async () => {
    const t = load({
        respond: () => ({ok: false, body: {}}),
        postsInThread: {p3: ['r1'], s1: ['p2']},
        known: {...posts, r1: {...posts.r1, root_id: ''}},
    });
    await t.socket('moved', {root_id: '', channel_id: 'c1', post_ids: ['r1'], source_roots: ['p3']});

    assert.deepEqual(t.dispatched.map(a => [a.type, a.data.id, a.data.root_id]),
        [['POST_REMOVED', 'r1', 'p3'], ['POST_REMOVED', 'r1', '']]);
});

test('a channel loaded back to its first post takes even posts older than it', async () => {
    const t = load({respond: arrivedAt(10, 20), postsInChannel: {c2: [{order: ['far'], recent: true, oldest: true}]}});
    await t.socket('moved', {root_id: '', channel_id: 'c2', post_ids: ['p1', 'p2'], source_roots: []}, 'c2');

    assert.deepEqual(types(t.dispatched), ['POST_REMOVED', 'POST_REMOVED', 'RECEIVED_POSTS', 'RECEIVED_POSTS_IN_CHANNEL']);
    const block = t.dispatched[3];
    assert.equal(block.channelId, 'c2');
    assert.deepEqual(plain(block.data.order), ['far', 'p1']);
    assert.equal(block.recent, true);
    assert.equal(block.oldest, true);
});

test('posts moved into an empty channel this screen has open appear there', async () => {
    const t = load({respond: byIds, postsInChannel: {c2: [{order: [], recent: true}]}});
    await t.socket('moved', {root_id: '', channel_id: 'c2', post_ids: ['p1', 'p2'], source_roots: []}, 'c2');

    const block = t.dispatched[3];
    assert.equal(block.type, 'RECEIVED_POSTS_IN_CHANNEL');
    assert.deepEqual(plain(block.data.order), ['p1']);
    assert.equal(block.recent, true);
    assert.equal(block.oldest, false);
});
