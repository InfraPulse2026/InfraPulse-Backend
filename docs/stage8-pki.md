# Этап 8 — регистрация, обновление и отзыв сертификатов Agent

## Что теперь работает

Backend обслуживает отдельный HTTPS listener для `/enroll` и `/renew`.
Администратор создаёт одноразовый токен для конкретного UUID агента. Agent
сам генерирует закрытый ключ и подписанный CSR, получает сертификат и подключается
к gRPC через mTLS. Закрытый ключ Agent никогда не передаётся Backend.

При обновлении Agent предъявляет текущий сертификат вместо bootstrap token.
Backend проверяет UUID, подпись/цепочку, срок и registry в PostgreSQL, выпускает
новый сертификат. Текущий Agent уже умеет сохранить bundle и переподключиться
с обновлёнными credentials. Его исходники и protobuf в этом этапе не менялись.

Это работающая реализация пункта PKI для нашего MVP, не заключение о проведённом
внешнем аудите промышленного развёртывания. Границы и проверенные сценарии ниже.

## Как проходит регистрация

1. У Agent есть постоянный UUID в `state_dir/agent-id`. Он сохраняется при
   обычном перезапуске; hostname не служит идентификатором.
2. Admin вызывает POST `/api/pki/bootstrap-tokens` с этим UUID. TTL по умолчанию
   15 минут, допустимы 60..86400 секунд. Токен содержит 256 случайных бит.
3. Ответ содержит token только один раз. ID токена генерируется независимо
   от секрета. В БД хранится SHA-256, а не исходный token.
4. Agent получает token через переменную окружения, указанную в `token_env`.
   Он проверяет HTTPS сервер по заранее предоставленному CA и server_name.
5. `/enroll` принимает Agent JSON: `agent_id` и `csr` — base64 PEM bytes.
   Authorization: `Bearer ip_enroll_...`. CSR должен иметь ECDSA P-256,
   валидную подпись и единственный URI `spiffe://infrapulse/agent/<UUID>`.
6. Backend создаёт ClientAuth сертификат, затем в одной транзакции проверяет
   неиспользованный токен, UUID, expiry/revocation, погашает token, регистрирует
   сертификат и пишет audit. Ответ возвращается только после COMMIT.
7. Ответ: `certificate` и `ca`, оба base64 PEM bytes, ровно как ожидает Agent.
8. Controller сверяет UUID в Hello и сертификате, затем fingerprint с registry.
   Только после успеха сохраняется регистрация и отправляется HelloAck.

Восемь параллельных enrollment запросов с одним токеном дают ровно один успех.
Если SQL запись сертификата или audit падает, транзакция возвращает token в
неиспользованное состояние. Если COMMIT завершился, но ответ потерялся в сети,
повторный enrollment запрещён: администратор должен выдать новый token.
Неиспользованные лишние tokens можно отозвать.

## Обновление и отзыв

`/renew` требует проверенный клиентский сертификат HTTPS соединения; Bearer
bootstrap token на этом endpoint не принимается. UUID CSR, сертификата и записи
registry должны совпасть. Старый сертификат остаётся действительным до expiry
или явного отзыва: потеря ответа renewal не должна лишать Agent подключения.

Registry хранит fingerprint, serial, UUID, сроки, renewed_from и revoked_at.
PEM client certificates и закрытые ключи не сохраняются в БД. Известный CA
сертификат, отсутствующий в registry, не разрешает подключение в PKI_ENABLED.

При отзыве registry и audit меняются атомарно. Затем Controller закрывает
готовый stream этого агента. Каждое следующее сообщение и отправка команды
также проверяют registry. Reconnect с отозванным сертификатом запрещён.
Если у Agent уже есть другой активный сертификат, он сможет переподключиться
с ним. Отзыв относится к конкретному fingerprint, не автоматически ко всем
сертификатам UUID. Для полного отключения отзывайте все активные credentials
и ещё не использованные bootstrap tokens этого UUID.

Максимум 16 действующих сертификатов и 16 неиспользованных активных tokens
на UUID. Истёкшие/отозванные записи не занимают эти лимиты. Публичный HTTPS
endpoint допускает до 120 запросов/минуту суммарно и 8 одновременных операций.
Это защита одного MVP instance, не распределённый лимитер для cluster.

## REST API администратора

Все маршруты требуют admin. Viewer/operator не могут выпускать или отзывать
credentials. Cookie-запросы на изменение требуют X-CSRF-Token; также можно
использовать Bearer session из `/api/auth/token`. Транзакция повторно проверяет,
что admin/session ещё действуют. В development bypass создавать tokens нельзя.

| Метод и маршрут | Результат |
|---|---|
| POST `/api/pki/bootstrap-tokens` | 201, token один раз; body: agent_id, ttl_seconds |
| GET `/api/pki/bootstrap-tokens` | metadata без секрета/хеша |
| POST `/api/pki/bootstrap-tokens/{id}/revoke` | отозвать неиспользованный token |
| GET `/api/pki/certificates` | fingerprints/serials/сроки/связь обновления/отзыв |
| POST `/api/pki/certificates/{fingerprint}/revoke` | отозвать сертификат |
| GET `/api/pki/audit` | enrollment, renewal и действия администратора |

GET поддерживает agent_id и limit 1..200, по умолчанию100. Ответы `{items:[...]}`,
сначала новые записи. Для revoke body строго `{"confirmed":true}`. Ответ 409
означает использованный/отозванный credential или достигнутый лимит; 503 — PKI
выключен либо БД недоступна. Ответы management и enrollment имеют no-store.
OpenAPI основного REST API обновлён до версии0.10.0.

## Локальный запуск на Windows, по шагам

Команды выполняйте из корня backend. Go не загружает `.env` автоматически:
для `go run` используйте `$env:...`; `.env` предназначен для Compose.

1. Поднимите только PostgreSQL из текущего Compose:

```powershell
docker compose up -d postgres
```

2. Создайте credentials в НОВОЙ папке вне backend:

```powershell
go run ./cmd/pki init-ca --directory ../infrapulse-pki-private --hosts localhost,127.0.0.1
```

CLI создаёт ca.pem, ca.key, server.pem, server.key. Существующую папку не
перезаписывает. CA действует5лет, server certificate1год. Не меняйте CA при
каждом запуске: Agent должен продолжать доверять прежнему CA. Windows не применяет
Unix mode0600 как ACL — проверьте права папки средствами Windows и оставьте доступ
только своему пользователю/учётной записи сервиса. Не отправляйте key файлы в чат,
архив проекта или GitHub. В ZIP credentials отсутствуют.

3. В этом окне задайте конфигурацию и запустите Backend:

```powershell
$env:PKI_ENABLED="true"
$env:PKI_CA_FILE="../infrapulse-pki-private/ca.pem"
$env:PKI_CA_KEY_FILE="../infrapulse-pki-private/ca.key"
$env:TLS_CERT_FILE="../infrapulse-pki-private/server.pem"
$env:TLS_KEY_FILE="../infrapulse-pki-private/server.key"
$env:TLS_CLIENT_CA_FILE="../infrapulse-pki-private/ca.pem"
go run ./cmd/server
```

Адреса по умолчанию: REST127.0.0.1:8080, gRPC127.0.0.1:9443,
enrollment HTTPS127.0.0.1:9444. Default certificate TTL720h (30суток).
PKI_ENABLED по умолчаниюfalse, поэтому прежний development запуск сохраняется.
PKI требует AUTH_MODE=required, gRPC mTLS, совпадающий issuing CA в client trust
и отдельные CA/key. PKI_TLS_CERT_FILE/KEY_FILE могут задать иной server pair для
HTTPS; по умолчанию используется gRPC pair. Сертификат должен содержать имя,
которое проверяет Agent, в DNS/IP SAN. Добавляйте реальные адреса при init-ca.

4. Во втором окне backend, если первый admin ещё не создан:

```powershell
go run ./cmd/access init-admin --username oleg --generate-password
```

Сохраните выданный пароль. CLI не перезапускает миграции и не сбрасывает live
сессии Agent. Если меняли DATABASE_URL, задайте одинаковое значение в обоих окнах.

5. Войдите через POST `/api/auth/token`; передавайте полученный access_token
как Bearer для management API. Получите UUID у разработчика Agent из его
постоянного agent-id. Для совершенно нового Agent identity создаётся перед
enrollment; первый запуск без токена создаст identity и завершится отказом
регистрации, после чего UUID можно прочитать. Не удаляйте существующий state.

6. Создайте token body:

```json
{"agent_id":"ВАШ-КАНОНИЧЕСКИЙ-UUID","ttl_seconds":900}
```

7. Передайте разработчику Agent публичный ca.pem, HTTPS URL, gRPC адрес,
server_name и одноразовый token по приватному каналу. Применяется уже
существующая конфигурация Agent, например:

```yaml
controller:
  address: localhost:9443
security:
  mode: production
  server_name: localhost
  renew_before: 168h
  enrollment:
    enabled: true
    url: https://localhost:9444
    ca_file: /absolute/path/to/ca.pem
    server_name: localhost
    token_env: INFRAPULSE_ENROLLMENT_TOKEN
    timeout: 10s
```

Agent developer задаёт token_env и запускает свой Agent обычным способом.
При перезапуске Agent использует сохранённый private bundle, не bootstrap token.
Для разных машин localhost заменяется реальным DNS/IP; при этом настройте
listeners/firewall и TLS SAN осознанно. Пример выше работает на одной машине.

## Docker overlay

Подготовленные credentials подключаются read-only через docker-compose.pki.yml:

```powershell
$env:PKI_SECRETS_DIR=(Resolve-Path ../infrapulse-pki-private).Path
docker compose -f docker-compose.yml -f docker-compose.pki.yml up --build -d
```

Образ работает от UID/GID10001. На Linux обеспечьте этому пользователю чтение
секретов: например, выделенная группа10001, папка0750, файлы0640; не выставляйте
мировое чтение key. На Windows проверьте read access через Docker Desktop mount.
CA/key должны оставаться вне image build context: .dockerignore исключает
pki-secrets, *.key, *.pem. Overlay публикует HTTPS9444 только на loopback.

Docker runtime в текущей среде не запускался: проверены Go-сборки и прямые
процессные сценарии, не реальный Docker deployment. Также требуется отдельная
настройка HTTPS reverse proxy для удалённого REST management: TLS Agent не
шифрует обычный HTTP REST listener. Существующие dev пароли БД замените при
реальном развёртывании, как описано в access-stage5.md.

## Что передаётся и что остаётся закрытым

При регистрации по HTTPS: UUID, публичный CSR и одноразовый token. При renewal:
UUID/CSR и TLS клиентский сертификат. По mTLS gRPC: обычные inventory, telemetry,
logs, audit, topology и результаты команд. Эти данные могут раскрывать устройство
сети; TLS защищает транспорт, не делает данные анонимными. Agent private key
не выходит с Agent. Issuing CA key хранится только у Backend/в secret mount.

В БД persist только хеш token и certificate metadata; журнал management не
содержит raw tokens, CSR или private keys. При утрате CA key нужен отдельный
план замены доверия на всех агентах. Автоматического CA rotation, CRL/OCSP,
HSM/Vault интеграции и нескольких одновременно работающих Controller здесь нет.
Сервис требует доступную БД и отклоняет credential при ошибке проверки.

## Фактически выполненные проверки

Unit/API: CSR signature/SAN/UUID, ClientAuth-only issuance, срок CA/leaf,
запрет plaintext enrollment и renewal без сертификата, role/CSRF/no-store,
новая папка CA без перезаписи, совпадение client trust, настройки PKI.

PostgreSQL: конкурентный enrollment, hash-only token, сохранение после startup,
renewal/отзыв, запрет чужого UUID и сессии admin после logout, used/revoked/expired
tokens, лимиты16, атомарное возвращение token при SQL ошибке certificate/audit.

Process integration запускает настоящий неизменённый P3 Agent с temporary
state/YAML, собственным CA, HTTPS, gRPC mTLS, REST auth и изолированной schema:
enroll → heartbeat → встроенный автоматический renewal через минуту → доказанное
подключение с новым TLS fingerprint → перезапуск без token → административный
отзыв → stream закрыт и revoked reconnect отклонён. Session ID внутри процесса
при reconnect не меняется: тест проверяет сам сертификат, не предполагает новый
session ID. Все fixture credentials удаляются вместе с временной папкой теста.

Обычный certificate lifetime в deploy30суток; fixture90секунд применяется только
для быстрого доказательства ротации. Native Linux runtime, внешний reverse proxy,
реальное сетевое железо и нагрузочное промышленное тестирование не заявляются.
