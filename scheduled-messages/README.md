# Scheduled Messages

`/schedule` 명령으로 메시지를 정해 둔 시각에 보내는 Mattermost 플러그인입니다.

원본은 [apartmentlines/mattermost-plugin-poor-mans-scheduled-messages](https://github.com/apartmentlines/mattermost-plugin-poor-mans-scheduled-messages) v1.3.1(커밋 `98321f3`)이고, 라이선스는 원본 그대로 [Apache License 2.0](LICENSE)입니다. 원본 설명서는 [UPSTREAM-README.md](UPSTREAM-README.md)에 있습니다. 이 폴더는 원본에 아래 두 가지를 더했습니다.

## 원본과 다른 점

- **`/schedule delete <번호|id>`**: `/schedule list`의 각 메시지 위에 `#1 · 0f6a9c2e`처럼 번호와 짧은 id가 붙습니다. 그 번호나 id(앞 8글자 이상)로 예약을 지웁니다. 자기 예약만 지울 수 있고, 목록의 Delete 버튼과 같은 소유자 검사를 거칩니다. 결과는 명령 응답으로 돌아오므로 API로 명령을 부르는 봇도 지울 수 있습니다. 번호는 메시지가 보내지거나 지워지면 바뀌니, 번호로 지울 때는 목록을 다시 보고 지웁니다.
- **기본 시간대 설정**: 프로필에 시간대가 없는 사용자(봇 계정 등)는 UTC 대신 플러그인 설정의 `Default timezone`을 씁니다. 비어 있으면 `Asia/Seoul`입니다. 프로필에 시간대가 있으면 원본처럼 그 값을 씁니다. 프로필 시간대나 설정값이 잘못된 이름이면 UTC로 잡습니다.

## 기존 예약과의 호환

플러그인 ID(`com.mattermost.plugin-poor-mans-scheduled-messages`)와 KV 저장 형식(키 접두사 `schedmsg:`·`user_sched_index:`, `types.ScheduledMessage`의 JSON)은 원본과 같습니다. 그래서 원본 위에 덮어 설치해도 이미 걸어 둔 예약을 그대로 읽고 보냅니다. 이미 걸린 예약의 시각과 시간대는 바뀌지 않습니다.

## 빌드와 설치

```sh
make test     # 테스트
make verify   # 검사와 배포 파일 생성
make dist     # dist/com.mattermost.plugin-poor-mans-scheduled-messages-1.3.1-ryude.1.tar.gz
```

System Console → Plugin Management에서 위 파일을 올리고 켭니다. 버전 `1.3.1-ryude.1`은 semver상 원본 `1.3.1`보다 낮으므로, 원본이 깔린 서버에는 덮어쓰기(`mmctl plugin add --force`)로 올립니다.
