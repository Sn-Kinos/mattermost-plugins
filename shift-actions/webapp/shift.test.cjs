const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const {JSDOM} = require('jsdom');

// Mattermost's Korean strings for the message ids the plugin uses.
const ko = {
    'forward_post_button.label': '전달하기', 'threading.threadMenu.followMessage': '메시지 구독',
    'threading.threadMenu.unfollow': '글타래 구독 해제', 'post_info.unread': '안 읽음 표시',
    'post_info.post_reminder.menu': '알림', 'post_info.pin': '공지하기', 'post_info.unpin': '공지 해제하기',
    'post_info.copy': '텍스트 복사', 'post_info.permalink': '링크 복사', 'post_info.edit': '편집', 'post_info.del': '삭제',
};

function load({roles = 'system_user', crt = 'on', following = false} = {}) {
    const dom = new JSDOM('<ul class="post-menu"><li id="root"></li><li><button id="CENTER_button_p1"></button></li></ul>',
        {runScripts: 'outside-only', url: 'https://chat.example/team-a/channels/town'});
    const {window} = dom;
    global.window = window;
    global.document = window.document;
    global.IS_REACT_ACT_ENVIRONMENT = true;
    window.React = require('react');
    window.ReactIntl = {useIntl: () => ({formatMessage: ({id, defaultMessage}) => ko[id] || defaultMessage})};
    const state = {entities: {
        users: {currentUserId: 'me', profiles: {me: {id: 'me', roles}}},
        general: {config: {CollapsedThreads: 'default_off'}},
        preferences: {myPreferences: {'display_settings--collapsed_reply_threads': {value: crt}}},
        threads: {threads: {p1: {is_following: following}}},
    }};
    // The hidden More button opens a menu with these items, like Mattermost's dot menu.
    const menu = ['forward_post_', 'unread_post_', 'remind_post_', 'pin_post_', 'copy_', 'permalink_', 'edit_post_', 'delete_post_'];
    const events = [];
    window.document.getElementById('CENTER_button_p1').addEventListener('click', () => {
        events.push('open:' + window.document.body.className);
        for (const id of menu) {
            const item = window.document.createElement('li');
            item.id = id + 'p1';
            item.tabIndex = -1;
            item.addEventListener('click', () => {
                events.push('click:' + item.id);
                window.document.getElementById('CENTER_button_p1').focus();
                if (id !== 'delete_post_') return;
                const confirm = window.document.createElement('button');
                confirm.id = 'deletePostModalButton';
                confirm.addEventListener('click', () => events.push('confirm:' + window.document.body.className));
                window.document.body.append(confirm);
            });
            item.addEventListener('keydown', event => events.push('key:' + item.id + ':' + event.key));
            window.document.body.append(item);
        }
    });
    window.document.addEventListener('keydown', event => event.key === 'Escape' && events.push('escape'));
    let Component;
    window.registerPlugin = (id, plugin) => plugin.initialize({
        registerPostActionComponent(component) { Component = component; },
    }, {getState: () => state, subscribe: () => () => {}});
    window.eval(fs.readFileSync(`${__dirname}/plugin.js`, 'utf8'));
    const {createRoot} = require('react-dom/client');
    const {act} = require('react-dom/test-utils');
    const root = createRoot(window.document.getElementById('root'));
    const render = post => act(() => root.render(window.React.createElement(Component, {post})));
    const key = (type, shiftKey) => act(() => { window.dispatchEvent(new window.KeyboardEvent(type, {key: 'Shift', shiftKey})); });
    const labels = () => [...window.document.querySelectorAll('#root button')].map(b => b.getAttribute('aria-label'));
    const click = label => act(async () => {
        window.document.querySelector(`#root button[aria-label="${label}"]`).click();
        await new Promise(resolve => setTimeout(resolve, 600));
    });
    return {window, events, render, key, labels, click, act};
}

const mine = {id: 'p1', user_id: 'me', root_id: '', is_pinned: false};
const theirs = {id: 'p1', user_id: 'other', root_id: '', reply_count: 2, is_pinned: true};

test('buttons appear only while Shift is held and hide every original toolbar item', async () => {
    const t = load();
    await t.render(mine);
    assert.deepEqual(t.labels(), []);
    await t.key('keydown', true);
    assert.deepEqual(t.labels(), ['전달하기', '메시지 구독', '안 읽음 표시', '알림', '공지하기', '텍스트 복사', '링크 복사', '편집', '삭제']);
    const style = [...t.window.document.querySelectorAll('style')].map(s => s.textContent).join('');
    assert.match(style, /\.post-menu:has\(\.shift-actions\) > li:not\(:has\(\.shift-actions\)\)/);
    await t.key('keyup', false);
    assert.deepEqual(t.labels(), []);
    await t.key('keydown', true);
    await t.act(() => { t.window.dispatchEvent(new t.window.Event('blur')); });
    assert.deepEqual(t.labels(), []);
});

test("others' posts hide edit and delete unless system admin, and follow and pin state show", async () => {
    const t = load({following: true});
    await t.render(theirs);
    await t.key('keydown', true);
    assert.deepEqual(t.labels(), ['전달하기', '글타래 구독 해제', '안 읽음 표시', '알림', '공지 해제하기', '텍스트 복사', '링크 복사']);
    const admin = load({roles: 'system_user system_admin', crt: 'off'});
    await admin.render(theirs);
    await admin.key('keydown', true);
    assert.deepEqual(admin.labels(), ['전달하기', '안 읽음 표시', '알림', '공지 해제하기', '텍스트 복사', '링크 복사', '편집', '삭제']);
});

test('menu buttons click the matching item in the hidden More menu and release its focus', async () => {
    const t = load();
    await t.render(mine);
    await t.key('keydown', true);
    await t.click('편집');
    assert.deepEqual(t.events, ['open:shift-actions-proxy', 'click:edit_post_p1']);
    assert.equal(t.window.document.body.className, '');
    assert.equal(t.window.document.activeElement, t.window.document.body);
});

test('remind opens the visible menu at its submenu and releases focus when it closes', async () => {
    const t = load();
    await t.render(mine);
    await t.key('keydown', true);
    await t.click('알림');
    assert.deepEqual(t.events, ['open:', 'key:remind_post_p1:ArrowRight']);
    t.window.document.getElementById('CENTER_button_p1').focus();
    assert.equal(t.window.document.activeElement, t.window.document.body);
});

test('a missing menu item closes the menu', async () => {
    const t = load();
    await t.render(theirs);
    await t.key('keydown', true);
    await t.click('공지 해제하기');
    assert.deepEqual(t.events, ['open:shift-actions-proxy', 'escape']);
    assert.equal(t.window.document.body.className, '');
});

test('delete confirms its dialog invisibly', async () => {
    const t = load();
    await t.render(mine);
    await t.key('keydown', true);
    await t.click('삭제');
    assert.deepEqual(t.events, ['open:shift-actions-proxy', 'click:delete_post_p1', 'confirm:shift-actions-proxy']);
    assert.equal(t.window.document.body.className, '');
});
