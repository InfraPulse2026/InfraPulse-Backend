# Backend v2 B0 — исходная база и безопасный переход

Дата: 2026-10-10. Основа — утверждённый полный план B0–B16 и локальный Stage9F.
Этот документ фиксирует фактические проверки, ограничения и порядок изменений.

## Сохранённая база

Рабочая ветка: `feature/backend-v2-b0-baseline`. Основа Git:
`a960eb676c3a036397d0c375ccfd0851fb09d9a9`. Stage1–Stage9F представлены
незакоммиченными изменениями; создание ветки не заменяет commit.
Коммиты, push и merge в B0 не выполнялись.

Полный архив Stage9F сохранён отдельно до изменений B0:
`InfraPulse-Backend-Stage9F-AI-Analysis.zip`, SHA256
`A65029E0D6E72F86222F59B04D334FA06AAA2EEDE342B7705578BD572010EDCD`.
Это резервная копия исходников, а не резервная копия пользовательской БД.

## Карта реализации

| Область | Код | Что сохраняем |
|---|---|---|
| Протокол | api/proto, api/gen, internal/controller | Hello, heartbeat, stream, ACK после COMMIT |
| Приём | internal/storage/postgres.go | Атомарное сохранение и replay/dedup |
| Inventory/metrics | internal/storage, internal/monitoring | Agent-scoped device identity и старые API |
| Топология | internal/topology, storage/topology.go | Наблюдения, разрешение соседей, TTL |
| Availability | internal/availability | Свежесть отдельно от физической недоступности |
| Logs/audit | internal/logbook, storage/logbook* | Поиск, нормализация и business record dedup |
| SIP | internal/telephony | История, latency, качество наблюдения |
| Доступ | internal/access | Роли, session/CSRF, аудит |
| Команды | internal/commands, controller/command_dispatch.go | Typed actions, approval, uncertain без слепого повтора |
| Сертификаты | internal/pki | Enrollment/renew/revoke и проверка UUID |
| Проблемы | internal/health, internal/diagnosis | 11 rules, evidence, состояния и ограничения корреляции |
| Отчёты | internal/reports | Расчёт по наблюдениям, immutable snapshots |
| AI | internal/analysisai | Preview, consent, allowlist, структурированный ответ |

Маршруты: [матрица API](backend-v2-b0-api-matrix.md), OpenAPI 0.16.0.
71 защищённый маршрут зафиксирован в frozen fixture. Публичные auth/health
и отдельный PKI listener перечислены в матрице. Формат старых ответов не меняем.

## Контракт Collector

Доступная reference-копия Agent: `b8460b692d91effb2dbf15906984053bd8e76823`.
Её proto совпадает с backend, кроме `go_package`; проверка выполняется с
явным `AGENT_ROOT`. Заморожены дескрипторы P2 и Stage9F: тесты защищают номера,
типы, optional/oneof, значения enum и сервис Connect.

Название старого P3 в тестах означает прежний этап ACK/PKI/rollback. Оно
не доказывает совместимость с новой Phase 3 Syslog из утверждённого плана.
В доступном proto отсутствуют SourceRef и типизированная hardware-модель.
Актуальный Collector Phase 3–5 необходимо получить от владельца Agent.
Код Agent не изменяется. Номера новых полей не назначаются по догадке.

## Проверки B0

Команды выполняются из корня backend с Go 1.27.1.
Полный интеграционный запуск требует `TEST_DATABASE_URL`, `AGENT_ROOT` и
`AGENT_BINARY` неизменённого Agent. Без этих переменных часть проверок пропускается.

```powershell
gofmt -l cmd internal api/proto
go vet ./...
go build ./...
go test -count=1 ./...
# В среде с C-компилятором:
$env:CGO_ENABLED = '1'
go test -race ./...
```

Локально используется отдельный PostgreSQL 18 на loopback:55432 и изолированные
тестовые схемы; пользовательский PostgreSQL на 5432 не используется.
Race detector локально недоступен: CGO выключен, C-компилятор отсутствует.
Linux-сборка не означает прохождение Linux runtime/race-тестов.
CI расширен gofmt/vet/race, но до публикации его выполнение не подтверждено.
Docker отсутствует: Compose smoke-test остаётся открытым пунктом B0.
Реальный AI provider не вызывается интеграционными fixture-тестами.

Результат локального запуска 2026-10-10:

| Проверка | Результат |
|---|---|
| gofmt | Нет неформатированных файлов |
| go vet ./... | PASS |
| go build ./... Windows amd64 | PASS |
| go build ./... Linux amd64 | PASS, только cross-build |
| go test -count=1 ./... с PostgreSQL и Agent | PASS; integration 88.603s, storage 78.347s |
| Новые baseline tests после добавления fixtures | PASS; api/proto и internal/api |
| Совпадение доступного canonical Agent proto | PASS, AGENT_ROOT указан явно |
| Рабочее дерево reference Agent | Чистое, изменений нет |
| Оставшиеся изолированные схемы тестов | 0 |
| Race / Docker / Linux-only durable process E2E | Не выполнены в этой среде |

Статус B0: доступные локальные проверки выполнены. Полная приёмка B0 требует
Docker smoke-test и Linux race/process E2E; это открытые проверки, не PASS.

## Миграции v2

1. Сохранять миграции 001–016 без редактирования и новые версии добавлять после них.
2. Добавлять DataSource/Asset/SourceBinding поверх существующих таблиц. Пара
   `(agent_id,device_id)` и исходные сообщения продолжают существовать.
3. Backfill выполнять порциями с durable cursor и повторяемым результатом.
   Старые строки не удалять; временные nullable-связи вводить до заполнения.
4. Конфликт идентификаторов сохранять как конфликт. IP/hostname не дают
   автоматическое подтверждение совпадения физического оборудования.
5. Для новой проекции сохранить атомарный ACK или transactional outbox. Нельзя
   подтверждать сообщение до его надёжного сохранения.
6. Проверять чистую БД, upgrade populated Stage9F, повтор миграции и аварийное
   прекращение на каждом этапе. Перед изменением командной БД нужен её backup.
7. `Migrate` сейчас сбрасывает connected/session состояния при старте одного
   контроллера. Не вызывать его из backfill/connector tools и не запускать
   несколько backend, пока не реализовано ownership сессий.

## Следующий этап B1

Нужен актуальный архив/commit Collector с Phase 3 Syslog и Phase 4 Redfish;
IPMI только если Phase 5 готова. Сначала diff proto и проверка field-number
конфликтов, затем additive sync на стороне backend, валидация/projections и
E2E Inventory + Telemetry + Logs + Hardware + durable replay.

B2/B3 начинаются после подтверждения B1. Redfish/IPMI поступают через Collector.
Новые backend HTTP-клиенты для BMC в этом плане не создаются.
Действующие RBAC, PKI, incidents, reports и AI не переписываются в B0.
