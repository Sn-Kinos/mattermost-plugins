const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const {JSDOM} = require('jsdom');

function loadHook(storage, fetchResult) {
    const dom = new JSDOM('', {runScripts: 'outside-only', url: 'https://chat.example'});
    dom.window.document.cookie = 'MMCSRF=token';
    dom.window.React = {createElement() {}, Component: class {}};
    dom.window.ReactIntl = {injectIntl: component => component};
    const requests = [];
    dom.window.fetch = async (url, options) => {
        requests.push({url, options});
        if (fetchResult instanceof Error) throw fetchResult;
        return {ok: true, json: async () => fetchResult};
    };
    let hook;
    dom.window.registerPlugin = (id, plugin) => plugin.initialize({
        registerTranslations() {},
        registerAdminConsoleCustomSetting() {},
        registerSlashCommandWillBePostedHook(fn) { hook = fn; },
    }, {getState: () => ({storage: {storage}})});
    dom.window.eval(fs.readFileSync(`${__dirname}/plugin.js`, 'utf8'));
    return {hook, requests};
}

const plain = value => JSON.parse(JSON.stringify(value));
const files = {value: {fileInfos: [{id: 'file1'}, {id: 'file2'}]}};

test('/trans with draft files posts them to the plugin and consumes the command', async () => {
    const {hook, requests} = loadHook({comment_draft_root: files}, {text: ''});
    const result = plain(await hook('/trans 안녕하세요', {channel_id: 'channel', team_id: 'team', root_id: 'root'}));
    assert.deepEqual(result, {});
    assert.equal(requests.length, 1);
    assert.equal(requests[0].url, '/plugins/io.github.sn-kinos.translator/trans');
    assert.equal(requests[0].options.headers['X-CSRF-Token'], 'token');
    assert.deepEqual(JSON.parse(requests[0].options.body),
        {channel_id: 'channel', root_id: 'root', command: '/trans 안녕하세요', file_ids: ['file1', 'file2']});
});

test('commands without draft files and other commands pass through unchanged', async () => {
    const args = {channel_id: 'channel', team_id: 'team', root_id: ''};
    const {hook, requests} = loadHook({draft_channel: files, comment_draft_root: files}, {text: ''});
    assert.deepEqual(plain(await hook('/translate x', args)), {message: '/translate x', args});
    assert.deepEqual(plain(await hook('/away', args)), {message: '/away', args});
    const empty = loadHook({}, {text: ''});
    assert.deepEqual(plain(await empty.hook('/trans 안녕', args)), {message: '/trans 안녕', args});
    assert.equal(requests.length + empty.requests.length, 0);
});

test('server text or request failure keeps the draft by returning an error', async () => {
    const args = {channel_id: 'channel', team_id: 'team', root_id: ''};
    const busy = loadHook({draft_channel: files}, {text: '현재 번역 요청이 많습니다.'});
    assert.deepEqual(plain(await busy.hook('/trans 안녕', args)), {error: {message: '현재 번역 요청이 많습니다.'}});
    const failed = loadHook({draft_channel: files}, new Error('offline'));
    assert.ok(plain(await failed.hook('/trans 안녕', args)).error.message);
});
