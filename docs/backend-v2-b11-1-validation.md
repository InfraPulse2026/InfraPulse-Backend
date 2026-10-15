# B11.1 — проверки и границы готовности

Дата: 12.10.2026. Backend HEAD остаётся `a960eb676c3a036397d0c375ccfd0851fb09d9a9`; commits/push/GitHub публикация не выполнялись. Agent reference `215d33a4a5fbc2a12c57d31e516b0ad33f7ae574` не изменён. Предыдущий B10 ZIP сохранён.

## Выполненные проверки

- `go fmt ./...` и `go vet ./...` прошли.
- Полный `go test ./... -count=1 -timeout=12m` с изолированной PostgreSQL и неизменёнными Agent/Collector binaries прошёл; `internal/storage` — 371.299 секунды. Проверены прежние функции, upgrade миграций, B9 managed configuration, B10 и новые B11.1 сценарии. После уточнения nullable OpenAPI повторно прошли API/domain/config tests.
- Unit tests `internal/configdrift`: VLAN diff; безопасная перестановка scalar-команд/целых interface blocks; Cisco служебные заголовки; ACL и repeated commands сохраняют порядок; banner остаётся opaque; unsupported/слишком большой input не получает compliant; обрезка количества контекстов; секретные строки не выходят в comparison JSON; рекомендации не исполняемы.
- API/config tests: admin baseline, viewer read, operator comparison; закрытые bodies/query, confirmation, revisions, idempotency headers; restore endpoint отсутствует; freshness `1m..24h`; актуальный OpenAPI проходит проверку.
- PostgreSQL tests: approved baseline → snapshot VLAN 30 → drift → snapshot VLAN 20 → compliant; late re-comparison старого снимка не возвращает current state назад; history и replay; optimistic concurrency; чужой Asset/snapshot; stale/retarget/disabled/merged/changed Asset states; live roles, confirmation и revision guards; redacted secret strings; неизвестный синтаксис; reviewed VoIP context без доказанного port impact.
- Backend builds Windows/amd64 и Linux/amd64; Automation Worker Linux/amd64, CGO disabled — прошли.
- Canonical Agent/proto reproducibility check — прошёл; transport не расширялся.
- OpenAPI `0.32.0`: 1082 внутренних references разрешаются, 202 schema; 6 новых операций. Nullable baseline/latest/proposal описаны с явным nullable object.
- YAML v3 parser разбирает основной/automation Compose, collections и CI workflow. Это синтаксическая проверка, не Docker execution.
- Существующие Python Runner adapter contract tests B10 — прошли, используют явно fake Runner.

## Что добавлено в базу

Migration `033_configuration_drift.sql`: composite same-Asset snapshot constraint, immutable baseline versions, current baseline pointer и unique drift results. Expected configuration остаётся encrypted в B10 snapshot; public diff не содержит raw config. Worker проверяет schema 033 и не вызывает migration/reset Agent connections.

Реконструкция старой B2.3 схемы в disposable upgrade test обновлена только для снятия новых зависимостей перед повторной миграцией. Пользовательские таблицы или файлы Agent не удалялись.

## Открытая приёмка и следующий шаг

- **B11.1 проверен на fixtures.** Нет утверждения о фактически изменённом VLAN на физическом/виртуальном сетевом устройстве. Для этого нужны реальный Linux Runner и выбранное SSH-устройство из B10.
- Этот отчёт относится только к B11.1. В актуальной сборке B11.2 реализован отдельными policy/request/approval/verification маршрутами; см. [валидацию B11.2](backend-v2-b11-2-validation.md).
- Общий vendor-independent rollback, Git expected state, arbitrary configuration restore и универсальная семантика всех прошивок не реализованы.
- Normalizer v1 ограничен Cisco/EOS interface/scalar grammar и conservative VyOS lines. Неподдержанный синтаксис возвращает unknown; нельзя использовать такой результат как доказательство compliance.
- VoIP context — reviewed same-Asset association, сохранённая на момент сравнения. Это не reconstruction всей исторической topology/endpoint identity и не доказательство конкретного VLAN/service impact.
- Race detector и Docker image/device execution не запускались здесь без Linux/C toolchain/Docker; соответствующая реальная приёмка остаётся открытой. Сборка Go binary для Linux её не заменяет.

Подробный запуск и API: [B11.1 configuration drift](backend-v2-b11-1-configuration-drift.md).
