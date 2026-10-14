const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const {JSDOM} = require('jsdom');
const React = require('react');
const {act, Simulate} = require('react-dom/test-utils');
const {createRoot} = require('react-dom/client');
const ReactIntl = require('react-intl');

test('glossary editor preserves existing terms, edits/adds/deletes, validates before save and respects read-only', async () => {
    assert.ok(fs.existsSync(`${__dirname}/plugin.js`), 'webapp editor must exist');
    const dom = new JSDOM('<div id="root"></div>', {runScripts: 'outside-only'});
    global.window = dom.window;
    global.document = dom.window.document;
    global.IS_REACT_ACT_ENVIRONMENT = true;
    window.React = React;
    window.ReactIntl = ReactIntl;
    let translations;
    const settings = {};
    let Editor;
    window.registerPlugin = (id, plugin) => {
        assert.equal(id, 'io.github.sn-kinos.translator');
        plugin.initialize({
            registerTranslations(source) { translations = source; },
            registerSlashCommandWillBePostedHook() {},
            registerAdminConsoleCustomSetting(key, component) {
                settings[key] = component;
                if (key === 'SharedGlossary') Editor = component;
            },
        });
    };
    window.eval(fs.readFileSync(`${__dirname}/plugin.js`, 'utf8'));
    let save;
    let confirmationRequired = false;
    let value = ' 배포 = deployment \n\n출시 = launch';
    const root = createRoot(document.getElementById('root'));
    const props = {
        id: 'PluginSettings.Plugins.io\\.github\\.sn-kinos\\.translator.sharedglossary', value,
        disabled: false, setByEnv: false,
        onChange(id, next, confirm) { assert.equal(id, props.id); value = next; confirmationRequired = confirm; },
        registerSaveAction(fn) { save = fn; },
        unRegisterSaveAction(fn) { assert.equal(fn, save); save = undefined; },
    };
    let locale = 'ko';
    const localized = (component, componentProps) => React.createElement(ReactIntl.IntlProvider,
        {locale, messages: translations(locale)}, React.createElement(component, componentProps));
    const render = () => act(() => root.render(localized(Editor, props)));
    const inputs = () => [...document.querySelectorAll('input')];
    const click = (text) => act(() => Simulate.click([...document.querySelectorAll('button')].find(b => b.textContent.includes(text))));
    const edit = (index, next) => act(() => Simulate.change(inputs()[index], {target: {value: next}}));
    render();
    assert.deepEqual(inputs().map(i => i.value), ['배포', 'deployment', '출시', 'launch']);
    assert.equal(value, props.value, 'mount must not rewrite saved values');
    assert.ok(document.body.textContent.includes('공용 단어 사전'));
    assert.ok(!document.body.textContent.includes('Shared Glossary'));
    locale = 'en';
    render();
    assert.ok(document.body.textContent.includes('Shared Glossary'));
    assert.ok(!document.body.textContent.includes('공용 단어 사전'));
    assert.equal(inputs()[0].value, '배포', 'changing locale must preserve entered terms');
    locale = 'ko';
    render();
    edit(1, 'release');
    assert.equal(value, '배포 = release\n출시 = launch');
    props.value = value;
    render();
    click('추가');
    props.value = value;
    render();
    assert.equal(inputs().length, 6, 'parent feedback must preserve the blank draft row');
    edit(4, '검토');
    props.value = value;
    render();
    assert.equal(inputs()[4].value, '검토');
    assert.equal(confirmationRequired, true, 'invalid draft must block Mattermost before patchConfig');
    assert.ok((await save()).error, 'incomplete rows must prevent saving');
    locale = 'en';
    render();
    assert.equal((await save()).error.message, 'Enter both Korean and English.');
    assert.ok(!document.body.textContent.includes('한국어와 영어를 모두'));
    locale = 'ko';
    render();
    assert.equal((await save()).error.message, '한국어와 영어를 모두 입력하세요.');
    edit(5, 'review');
    assert.equal((await save()).error, undefined);
    assert.equal(confirmationRequired, false);
    assert.equal(value, '배포 = release\n출시 = launch\n검토 = review');
    edit(4, '배포');
    assert.ok((await save()).error, 'duplicate Korean keys must prevent saving');
    edit(4, '검토=확인');
    assert.ok((await save()).error, 'delimiter injection must prevent saving');
    edit(4, '가'.repeat(101));
    assert.ok((await save()).error, 'server Unicode limit must be enforced');
    edit(4, '😀'.repeat(100));
    assert.equal((await save()).error, undefined);
    click('삭제');
    assert.ok(!value.includes('배포 = release'));
    props.value = '새 값 = new value';
    render();
    assert.deepEqual(inputs().map(i => i.value), ['새 값', 'new value']);
    props.value = value;
    render();
    assert.equal(inputs()[0].value, '출시', 'external reset may equal an older emitted value');
    props.disabled = true;
    render();
    assert.ok([...document.querySelectorAll('input, button')].every(el => el.disabled));
    props.disabled = false;
    props.setByEnv = true;
    render();
    assert.ok([...document.querySelectorAll('input, button')].every(el => el.disabled));
    props.setByEnv = false;
    props.value = Array.from({length: 200}, (_, i) => `단어${i} = term${i}`).join('\n');
    render();
    assert.equal(inputs().length, 400);
    assert.equal([...document.querySelectorAll('button')].find(b => b.textContent.includes('추가')).disabled, true);
    edit(0, '');
    assert.equal(confirmationRequired, true);
    let cancelled = false;
    props.cancelSubmit = () => { cancelled = true; };
    props.showConfirm = true;
    render();
    assert.equal(cancelled, true);
    assert.equal(document.activeElement.getAttribute('role'), 'status');
    act(() => root.unmount());
    assert.equal(save, undefined);
    const secondRoot = createRoot(document.getElementById('root'));
    props.value = '미완성 = ';
    props.showConfirm = false;
    confirmationRequired = false;
    act(() => secondRoot.render(localized(Editor, props)));
    assert.equal(confirmationRequired, true, 'invalid saved data must also establish the pre-save gate');
    act(() => secondRoot.unmount());
    const settingsRoot = createRoot(document.getElementById('root'));
    const manifest = JSON.parse(fs.readFileSync(`${__dirname}/../plugin.json`, 'utf8'));
    for (const setting of manifest.settings_schema.settings.filter(s => s.key !== 'SharedGlossary')) {
        let changed;
        const fieldProps = {id: `config.${setting.key}`, value: setting.default ?? '', disabled: false,
            onChange: (id, value) => { changed = {id, value}; }};
        fieldProps.value = undefined;
        act(() => settingsRoot.render(localized(settings[setting.key], fieldProps)));
        const defaultInput = document.querySelector('input');
        if (setting.type === 'bool') assert.equal(defaultInput.checked, setting.default);
        else assert.equal(defaultInput.value, String(setting.default ?? ''));
        fieldProps.value = setting.default ?? '';
        for (const language of ['ko', 'en', 'fr']) {
            locale = language;
            act(() => settingsRoot.render(localized(settings[setting.key], fieldProps)));
            const label = document.querySelector('label');
            assert.equal(/[가-힣]/.test(label.textContent), language === 'ko');
            assert.ok(!label.textContent.includes(' / '));
            assert.equal(/[가-힣]/.test(document.querySelector('.help-text').textContent), language === 'ko');
        }
        const input = document.querySelector('input');
        const isBool = setting.type === 'bool';
        act(() => Simulate.change(input, {target: {value: setting.type === 'number' ? '9' : 'updated', checked: false}}));
        assert.deepEqual(changed, {id: fieldProps.id, value: isBool ? false : setting.type === 'number' ? 9 : 'updated'});
        if (setting.secret) assert.equal(input.type, 'password');
        fieldProps.setByEnv = true;
        act(() => settingsRoot.render(localized(settings[setting.key], fieldProps)));
        assert.equal(document.querySelector('input').disabled, true);
    }
    act(() => settingsRoot.unmount());
    dom.window.close();
});
