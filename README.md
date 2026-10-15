# Mattermost plugins

Mattermost 플러그인 모음입니다. 플러그인마다 폴더가 따로 있고, 각 폴더의 README에 설정과 설치 방법이 있습니다.

| 폴더 | 플러그인 ID | 하는 일 |
|---|---|---|
| [translator](translator/README.md) | `io.github.sn-kinos.translator` | 한국어·영어 메시지 번역 (`/trans`, 지정한 사용자의 메시지 자동 번역) |
| [shift-actions](shift-actions/README.md) | `io.github.sn-kinos.shift-actions` | Shift를 누르면 메시지 툴바를 더 보기 메뉴 항목으로 바꿈 |

```sh
make test     # 모든 플러그인 테스트
make verify   # 모든 플러그인 검사와 배포 파일 생성 (각 폴더의 dist/)
make -C translator dist   # 플러그인 하나만
```

`.github/workflows/ci.yml`이 push와 pull request마다 `make verify`를 돌립니다. 각 폴더의 `dist/`에 생긴 파일을 System Console → Plugin Management에서 올리면 됩니다.

## 라이선스

[MIT](LICENSE)
