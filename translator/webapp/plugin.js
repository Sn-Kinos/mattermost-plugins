/* Mattermost provides React; this file ships directly without a bundler. */
(() => {
    const React = window.React;
    const h = React.createElement;
    const prefix = 'io.github.sn-kinos.translator.';
    // Each pair is English (fallback), Korean. Mattermost chooses the user's locale.
    const messages = {
        GeminiAPIKey: ['Gemini API Key', 'Gemini API 키'],
        'GeminiAPIKey.help': ['API key used only by the server for Gemini requests.', 'Gemini 번역 요청에 사용할 API 키입니다. 서버에서만 사용합니다.'],
        PrimaryModel: ['Primary Model', '기본 모델'],
        'PrimaryModel.help': ['Gemini model ID to try first.', '번역에 먼저 사용할 Gemini 모델 ID입니다.'],
        FallbackModel: ['Fallback Model', '대체 모델'],
        'FallbackModel.help': ['Model ID used after primary retries fail.', '기본 모델의 재시도가 실패하면 사용할 모델 ID입니다.'],
        AutomaticTeam: ['Automatic Translation Team', '자동 번역 팀'],
        'AutomaticTeam.help': ['Enter the team name from its URL, not its display name or ID. Example: my-team.', '팀 URL에 쓰이는 팀명을 입력하세요. 예: my-team. 표시 이름이나 팀 ID가 아닙니다.'],
        AutomaticSourceUsers: ['Automatic Source Users', '자동 번역 대상 사용자'],
        'AutomaticSourceUsers.help': ['Comma-separated usernames whose messages are translated from English to Korean. Example: alex, sam.', '사용자명을 쉼표(,)로 구분하세요. 예: alex, sam. 지정 사용자의 메시지를 영어→한국어로 번역합니다.'],
        IncludeDirectMessages: ['Include Direct Messages', '다이렉트 메시지 포함'],
        'IncludeDirectMessages.help': ['Include DMs and group DMs when all active participants belong to the configured team.', '모든 활성 참여자가 위 팀에 속한 DM과 그룹 DM도 자동 번역합니다.'],
        SharedGlossary: ['Shared Glossary', '공용 단어 사전'],
        'SharedGlossary.help': ['Applies in both directions. Up to 200 entries, 100 characters per term.', '양방향 번역에 적용합니다. 최대 200개, 각 언어 100자까지 입력할 수 있습니다.'],
        MaximumConcurrentGeminiRequests: ['Maximum Concurrent Gemini Requests', '최대 동시 번역 요청'],
        'MaximumConcurrentGeminiRequests.help': ['Concurrent Gemini request limit (1–64), shared by automatic and command translations.', '동시에 처리할 Gemini 요청 수입니다(1~64). 자동 번역과 명령어 번역이 함께 사용합니다.'],
        TotalRequestTimeoutSeconds: ['Total Request Timeout Seconds', '전체 요청 제한 시간(초)'],
        'TotalRequestTimeoutSeconds.help': ['Total timeout including retries and fallback. Must be 10 seconds in this version.', '재시도와 대체 모델을 포함한 전체 제한 시간입니다. 현재 버전에서는 10초로 설정해야 합니다.'],
        korean: ['Korean', '한국어'], english: ['English', '영어'],
        empty: ['No entries yet. Add a term below.', '등록된 단어가 없습니다. 아래에서 단어를 추가하세요.'],
        add: ['+ Add term', '+ 단어 추가'], delete: ['Delete', '삭제'],
        row: ['Row {number}: {language}', '{number}행 {language}'],
        deleteRow: ['Delete row {number}', '{number}행 삭제'],
        koreanExample: ['e.g. 배포', '예: 배포'], englishExample: ['e.g. deployment', '예: deployment'],
        save: ['Save changes below.', '변경 후 아래 저장 버튼을 누르세요.'],
        limit: ['Maximum 200 entries.', '단어는 최대 200개까지 등록할 수 있습니다.'],
        incomplete: ['Enter both Korean and English.', '한국어와 영어를 모두 입력하세요.'],
        delimiter: ['No equals signs or line breaks.', '단어에 등호(=)나 줄바꿈을 넣을 수 없습니다.'],
        length: ['Maximum 100 Unicode characters per term.', '각 단어는 100자 이내로 입력하세요.'],
        duplicate: ['Korean terms must be unique.', '같은 한국어 단어를 중복 등록할 수 없습니다.'],
    };
    const translate = (intl, key, values) => intl.formatMessage({id: prefix + key, defaultMessage: messages[key][0]}, values);

    class GlossaryEditor extends React.Component {
        text(key, values) { return translate(this.props.intl, key, values); }
        constructor(props) {
            super(props);
            this.nextID = 0;
            this.state = {rows: this.parse(props.value)};
            this.validateSave = async () => {
                const message = this.validationError();
                return message ? {error: {message}} : {};
            };
        }

        parse(value) {
            return String(value || '').split('\n').filter(line => line.trim()).map(line => {
                const separator = line.indexOf('=');
                return {
                    id: this.nextID++,
                    korean: (separator < 0 ? line : line.slice(0, separator)).trim(),
                    english: separator < 0 ? '' : line.slice(separator + 1).trim(),
                };
            });
        }

        componentDidMount() {
            this.props.registerSaveAction(this.validateSave);
            this.guardInvalidSavedValue();
        }

        componentDidUpdate(previous) {
            if (previous.value !== this.props.value && this.props.value !== this.emittedValue) {
                this.emittedValue = this.props.value;
                this.setState({rows: this.parse(this.props.value)}, () => this.guardInvalidSavedValue());
            }
            if (this.props.showConfirm && !previous.showConfirm && this.validationError()) {
                document.getElementById(`${this.props.id}-error`)?.focus();
                this.props.cancelSubmit();
            }
        }

        componentWillUnmount() {
            this.props.unRegisterSaveAction(this.validateSave);
        }

        guardInvalidSavedValue() {
            if (!this.props.disabled && !this.props.setByEnv && this.validationError()) {
                this.props.onChange(this.props.id, this.props.value, true);
            }
        }

        change(rows) {
            if (this.props.disabled || this.props.setByEnv) {
                return;
            }
            this.setState({rows});
            this.emittedValue = rows.filter(row => row.korean.trim() || row.english.trim())
                .map(row => `${row.korean.trim()} = ${row.english.trim()}`).join('\n');
            // Mattermost runs save actions AFTER patchConfig. Its confirmation gate
            // must block invalid drafts before the configuration request is sent.
            this.props.onChange(this.props.id, this.emittedValue, Boolean(this.validationError(rows)));
        }

        validationError(draft = this.state.rows) {
            const rows = draft.filter(row => row.korean.trim() || row.english.trim());
            if (rows.length > 200) {
                return this.text('limit');
            }
            const seen = new Set();
            for (const row of rows) {
                const korean = row.korean.trim();
                const english = row.english.trim();
                if (!korean || !english) {
                    return this.text('incomplete');
                }
                if (/[=\r\n]/.test(korean + english)) {
                    return this.text('delimiter');
                }
                if ([...korean].length > 100 || [...english].length > 100) {
                    return this.text('length');
                }
                if (seen.has(korean)) {
                    return this.text('duplicate');
                }
                seen.add(korean);
            }
            return '';
        }

        render() {
            const disabled = this.props.disabled || this.props.setByEnv;
            const error = this.validationError();
            const rowStyle = {display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) minmax(0, 1fr) auto', gap: 8, marginBottom: 8};
            return h('div', {id: this.props.id, role: 'group', 'aria-label': this.text('SharedGlossary')},
                h('div', {style: {...rowStyle, fontWeight: 600}},
                    h('span', null, this.text('korean')), h('span', null, this.text('english')), h('span', null, '')),
                this.state.rows.length === 0 ? h('p', {className: 'help-text'}, this.text('empty')) : null,
                ...this.state.rows.map((row, index) => h('div', {key: row.id, style: rowStyle},
                    ...['korean', 'english'].map(field => h('input', {
                        key: field, className: 'form-control', type: 'text', value: row[field], disabled,
                        'aria-label': this.text('row', {number: index + 1, language: this.text(field)}),
                        'aria-describedby': `${this.props.id}-error`,
                        placeholder: this.text(field + 'Example'),
                        onChange: event => this.change(this.state.rows.map(item => item.id === row.id ? {...item, [field]: event.target.value} : item)),
                    })),
                    h('button', {
                        type: 'button', className: 'btn btn-tertiary', disabled,
                        'aria-label': this.text('deleteRow', {number: index + 1}),
                        onClick: () => this.change(this.state.rows.filter(item => item.id !== row.id)),
                    }, this.text('delete')))),
                h('button', {
                    type: 'button', className: 'btn btn-tertiary', disabled: disabled || this.state.rows.length >= 200,
                    onClick: () => this.change([...this.state.rows, {id: this.nextID++, korean: '', english: ''}]),
                }, this.text('add')),
                h('p', {id: `${this.props.id}-error`, role: 'status', tabIndex: -1, className: error ? 'help-text error-text' : 'help-text'},
                    error || `${this.state.rows.length} / 200 · ${this.text('save')}`));
        }
    }

    // Mattermost drops draft attachments when it runs a slash command, so
    // /trans with files goes to the plugin endpoint together with their IDs.
    const transWithFiles = store => async (message, args) => {
        if (!/^\/trans(\s|$)/i.test(message)) return {message, args};
        const key = args.root_id ? 'comment_draft_' + args.root_id : 'draft_' + args.channel_id;
        const files = store.getState().storage?.storage?.[key]?.value?.fileInfos || [];
        if (!files.length) return {message, args};
        const csrf = document.cookie.match(/(?:^|;\s*)MMCSRF=([^;]+)/)?.[1] || '';
        try {
            const response = await fetch('/plugins/io.github.sn-kinos.translator/trans', {
                method: 'POST', credentials: 'same-origin',
                headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrf, 'X-Requested-With': 'XMLHttpRequest'},
                body: JSON.stringify({channel_id: args.channel_id, root_id: args.root_id || '', command: message,
                    file_ids: files.map(file => file.id)}),
            });
            if (!response.ok) throw new Error(String(response.status));
            const {text} = await response.json();
            return text ? {error: {message: text}} : {};
        } catch {
            return {error: {message: '메시지를 게시하지 못했습니다. 잠시 후 다시 시도해 주세요.'}};
        }
    };

    window.registerPlugin('io.github.sn-kinos.translator', {
        initialize(registry, store) {
            registry.registerSlashCommandWillBePostedHook(transWithFiles(store));
            registry.registerTranslations(locale => Object.fromEntries(Object.entries(messages)
                .map(([key, pair]) => [prefix + key, pair[locale.toLowerCase().split('-')[0] === 'ko' ? 1 : 0]])));
            const types = {GeminiAPIKey: 'password', PrimaryModel: 'text', FallbackModel: 'text',
                AutomaticTeam: 'text', AutomaticSourceUsers: 'text', IncludeDirectMessages: 'checkbox',
                SharedGlossary: 'glossary', MaximumConcurrentGeminiRequests: 'number', TotalRequestTimeoutSeconds: 'number'};
            const defaults = {PrimaryModel: 'gemini-3.7-flash', FallbackModel: 'gemini-3.6-flash',
                IncludeDirectMessages: true,
                MaximumConcurrentGeminiRequests: 8, TotalRequestTimeoutSeconds: 10};
            for (const [key, type] of Object.entries(types)) {
                const Setting = window.ReactIntl.injectIntl(props => {
                    const text = (id) => translate(props.intl, id);
                    const disabled = props.disabled || props.setByEnv;
                    const value = props.value ?? defaults[key] ?? '';
                    const control = type === 'glossary' ? h(GlossaryEditor, props) : h('input', {
                        id: props.id, type, className: type === 'checkbox' ? undefined : 'form-control', disabled,
                        value: type === 'checkbox' ? undefined : value,
                        checked: type === 'checkbox' ? Boolean(value) : undefined,
                        'aria-describedby': props.id + '-help',
                        onChange: event => {
                            if (disabled) return;
                            const value = type === 'checkbox' ? event.target.checked : type === 'number' ? Number(event.target.value) : event.target.value;
                            props.onChange(props.id, value);
                        },
                    });
                    return h('div', {className: 'form-group'},
                        h('label', {className: 'control-label col-sm-4', htmlFor: props.id}, text(key)),
                        h('div', {className: 'col-sm-8'}, control,
                            h('p', {id: props.id + '-help', className: 'help-text'}, text(key + '.help'))));
                });
                registry.registerAdminConsoleCustomSetting(key, Setting, {showTitle: false});
            }
        },
    });
})();
