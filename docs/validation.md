# Результаты проверок MVP

Актуальные P3 ACK/durable integration результаты от 6 октября 2026 года:
[reliable-delivery.md](reliable-delivery.md). Ниже сохранена история предыдущих
проверок MVP от 5 октября; она не описывает состояние P3 delivery после исправления.

Проверено локально 5 октября 2026 года на Windows с Go 1.27.1.
Для проверки базы запущен отдельный PostgreSQL 18 на `127.0.0.1:55432`,
с собственным каталогом данных. Существующий PostgreSQL пользователя
и его учебные данные не изменялись.

## Выполнено успешно

- `go fmt ./...` — форматирование;
- `go vet ./...` — статические проверки;
- `go test ./... -count=1` с `TEST_DATABASE_URL` — все пакеты прошли;
- `go build -o bin/server.exe ./cmd/server` — сборка Windows;
- `GOOS=linux CGO_ENABLED=0 go build -trimpath -o bin/server-linux ./cmd/server`
  — статическая сборка Linux;
- настоящий Simulator из неизменённого Agent → gRPC backend → PostgreSQL → REST;
- настоящий Agent в collector-only режиме → регистрация и heartbeat;
- доступность `/readyz`, `/api/agents`, `/api/devices`, `/telemetry`,
  `/metrics/history`, `/openapi.json` на запущенном backend;
- сверка SHA-256 всех 170 файлов исходного Agent с архивом — без изменений;
- отдельная сверка proto и двух generated Go files — точные копии.

## Проверенные сценарии

gRPC: Hello/HelloAck, правильная версия протокола, обязательные данные,
совпадение identity, растущий sequence, повторный активный stream одного Agent,
невалидные даты/NaN, отказ хранилища до HelloAck, чистое завершение stream.

PostgreSQL: атомарность batch при ошибке второго sample; повтор одинакового
message_id; конфликт содержимого под тем же ID; метрики вне порядка времени;
нулевой CPU против отсутствующего; полный диапазон uint64 для RAM; inventory
сохраняется при telemetry; одинаковые device_id у разных agents; история
сессий; heartbeat восстанавливает local online; timeout; старый disconnect
не закрывает новую сессию; повторное применение миграции.

HTTP: формы ответов, пустые массивы, 400/404/409/503, latest/history,
inventory, telemetry alias, OpenAPI. Тест жизненного цикла запускает настоящий
backend с временной схемой, ждёт `/readyz`, отменяет контекст и проверяет
завершение серверов без ошибки.

## Сквозное демо

Сценарий Simulator `all`, 10 секунд, 4 устройства, interval 1s:

| Тип | Сохранено envelopes |
|---|---:|
| hello | 1 |
| inventory | 10 |
| telemetry | 37 |
| topology | 40 |
| logs | 10 |
| events | 8 |
| telephony | 10 |

В `telemetry` сохранено 37 samples. В REST получены `source=simulated`,
CPU и RAM, история и последнее измерение. Количество samples меньше 40:
сценарий содержит период недоступности первого устройства, когда оно не
отправляет online telemetry.

Обычный Agent на Windows проверен с `local.enabled: false`: регистрация,
HelloAck, heartbeat с интервалом 10 секунд, изменение last_seen и
`connected=true` в API. Это проверка реального транспорта/жизненного цикла
Agent, а не доказательство сбора Windows CPU/RAM.

Последняя версия дополнительно проверена коротким `healthy` demo через
поставляемую `configs/agent.simulator.yaml`: REST отдаёт UTC timestamps,
`received_at`, `source=simulated` и CPU. Примеры Agent используют `127.0.0.1`,
поскольку `localhost` в этой среде не дал успешного gRPC-подключения к IPv4 listener.

## Дополнение: настраиваемые интервалы

Дополнение: проверены миграции 002/003, сохраняемые per-device интервалы 3s..24h,
изолированность collectors и независимость telemetry freshness от heartbeat.
Исходный Agent validate-config принимает 10h/24h и отклоняет 24h1s;
суточный runtime-цикл не запускался. Удалённое применение настроек в Agent
не реализовано. Подробности — [monitoring-intervals.md](monitoring-intervals.md).

На запущенном backend через PUT сохранён интервал telemetry 86400s;
GET вернул ту же политику, manual/unverified и корректную freshness.
Миграции 002 и 003 применены поверх существующей схемы 001 с сохранением старых
измерений. Проверены fresh/stale для суточного профиля; обе границы API, 3s/24h, проходят проверку. В отдельной тестовой базе сохранились все 41 прежнее измерение.

## Не проверено в первоначальной среде 5 октября

- Docker Compose runtime и сборка Dockerfile: Docker здесь не установлен.
  Предусмотрены проверки `docker compose config --quiet` и `docker compose build`
  в CI; CI ещё не запускался, так как код не отправлялся в GitHub.
- Реальный Linux Agent → backend с CPU/RAM/interfaces: Linux/WSL здесь нет.
  Готовая конфигурация `configs/agent.linux.yaml` и команды находятся в README.
- Локальный `go test -race`: компилятор C для Windows не найден; команда
  предусмотрена в Linux CI, но её прохождение здесь не заявляется.
- mTLS handshake, настоящие сетевые устройства, AI и frontend.

Это рабочий проверенный backend MVP в описанной локальной конфигурации,
с явно указанными границами проверок. Готовность всей платформы или
производственного развёртывания не заявляется.
