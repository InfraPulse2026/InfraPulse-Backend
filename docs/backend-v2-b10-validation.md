# B10 — проверки и границы приёмки

Проверено 12.10.2026 в локальной Windows-среде. Git HEAD backend остаётся `a960eb676c3a036397d0c375ccfd0851fb09d9a9`. Исходники Agent/Collector reference `215d33a4a5fbc2a12c57d31e516b0ad33f7ae574` не изменялись. Git commit/push, публикация GitHub, изменение frontend и презентации не выполнялись.

## Выполненные проверки

- `go fmt ./...`: форматирование выполнено.
- `go vet ./...`: прошло.
- Полный `go test ./... -count=1 -timeout=12m` с изолированной PostgreSQL и существующими Agent/Collector binaries: прошёл. `internal/storage` — 360.751 секунды; `internal/integration` — 130.423 секунды. Архив/дедупликация, migrations/upgrade, topology, incidents, logs, SIP, команды/rollback, PKI и B9 managed configuration сохранены.
- После общего прогона уточнены блокировки live identity/session, resource IDs в аудите, повторная отмена и проверка Asset/template/actor при сохранении результата. Повторно проверены все B10 PostgreSQL-сценарии, API, domain и config; они прошли. Добавленный тест operator ownership и config enablement также прошёл. Эти уточнения не меняют существующий Agent transport.
- Python-адаптер: `python -B scripts/test_automation_runner.py` — 2 теста прошли. Проверены все три шаблона, фиксированные команды/facts modules, запрет автоматического принятия SSH host key, скрытие error details и подавление сохранения raw Runner events. Здесь используется **явный fake Runner**, а не настоящее сетевое устройство.
- OpenAPI `0.31.0`: 1029 внутренних ссылок разрешаются; 191 schema. Все 11 новых операций описаны, входные schemas закрыты.
- YAML `docker-compose.yml`, `docker-compose.automation.yml`, `configs/automation-collections.yml`, `.github/workflows/check.yml` разбирается настоящим YAML v3 parser. Синтаксическая проверка не заменяет `docker compose config` и сборку образа.
- Backend собирается для Windows/amd64 и Linux/amd64; отдельный worker — для Linux/amd64, без CGO.
- `scripts/generate-proto.ps1 -Check` с reference Agent `215d33a`: прошёл; контракт и воспроизводимые generated files не изменились.

## Что именно проверяют PostgreSQL-сценарии

1. Job idempotency, конфликт повторного ключа и запрет второй активной задачи для одного Asset.
2. Два конкурентных claims возвращают задание только одному обработчику.
3. Сохранение успешного snapshot с правильным Asset/job, digest и размером. Чужой Asset в результате отклоняется.
4. Шифротекст не содержит открытой fixture-конфигурации; admin decrypt возвращает исходное содержимое.
5. Ошибочный ключ шифрования не принимается при запуске; прежний ciphertext не заменяется.
6. Queued/running cancellation, session revocation, смена target revision и merged Asset.
7. Timeout/orphan recovery, блокировка позднего результата по старому claim.
8. Неполный/ошибочный результат не сохраняет snapshot и не получает succeeded.
9. Отзыв пользователя во время выполнения блокирует последующее сохранение успеха.
10. Operator не меняет admin SSH bindings, не отменяет чужой job и не читает полную configuration; read-only job создавать может.
11. Аудит запуска содержит конкретный job ID; чтение snapshot регистрируется.
12. Проверка схемы обработчиком не отключает соединённый Agent.

Fixtures используют тестовые конфигурации и адреса `192.0.2.0/24`; это не результаты проверки популярных физических моделей.

## Открытые проверки

- **Реальный Ansible + сетевое устройство:** не запускался здесь. Windows не является Ansible control node; Docker отсутствует, WSL не установлен. Нужно выполнить Linux-сценарий из [инструкции B10](backend-v2-b10-ansible.md). Opt-in `TestAutomationRealLinuxRunnerSnapshot` пропускается явно без такой среды.
- **Docker build/Compose execution:** новый worker image и overlay написаны, но в текущей среде не собраны/не запущены. Versions основных пакетов/collections проверены по официальным PyPI/Galaxy; работоспособность их комбинации подтверждается на Linux.
- **Race detector:** локально не запускался без C toolchain; сохранён в CI. Для полного набора PostgreSQL/race увеличен CI timeout до 20 минут и добавлены worker build/Python adapter contract tests. GitHub CI ещё не запускался, потому что код не публиковался.
- **SSH host key mismatch и отмена настоящего Runner:** реализованы через pinned known_hosts и cancel callback/процесс, но нужен реальный Linux process/device сценарий. Проверка fake adapter не доказывает остановку всех настоящих subprocesses на конкретной версии runtime.
- **Production secret distribution, PostgreSQL TLS/least privilege, egress firewall, полная фиксация транзитивных зависимостей:** не являются завершённой production-приёмкой этого локального этапа.

Этап B10 имеет проверенную backend-реализацию, но его конечный критерий из плана — получение **фактической** конфигурации устройства через Ansible — считается подтверждённым только после real Linux/device test. B11 можно разрабатывать на этой модели snapshots, сохраняя явное различие между fixtures и реальными данными.
