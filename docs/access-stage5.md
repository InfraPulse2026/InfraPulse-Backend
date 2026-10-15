# Этап 5: пользователи, сессии и права REST API

Backend теперь проверяет, кто читает данные и кто изменяет настройки. Это
серверная часть входа: экран входа и остальные страницы разрабатывает отдельный
frontend-разработчик. Код Agent и его protobuf-контракт этот этап не меняет.

В этапах 1–4 REST был доступен без входа на локальном стенде. Теперь
`AUTH_MODE=required` включён по умолчанию. Без действующей сессии запросы к
данным `/api/...` получают 401; недостаточная роль получает 403. Публичными
остаются `/healthz`, `/readyz`, `/openapi.json` и два маршрута входа.

## 1. Что может каждый пользователь

| Роль | Просматривать данные | Менять ожидаемые интервалы устройств и SIP | Управлять пользователями и читать журнал доступа |
|---|---|---|---|
| `viewer` | Да | Нет | Нет |
| `operator` | Да | Да | Нет |
| `admin` | Да | Да | Да |

Просмотр включает устройства, telemetry, карту, доступность, логи, Linux audit
и телефонию. Изменение интервалов относится к backend policy: оно пока не
перестраивает scheduler Agent. Этап 6 дополнительно даёт operator/admin право
commands.write для подтверждённых разрешённых interface-команд; viewer только
читает команды. Флаги backend/Agent и preflight остаются обязательными независимо от роли.

Роли действуют на всю текущую установку InfraPulse. Ограничений по отдельным
Agent, устройствам, площадкам или организациям пока нет. Любая из трёх ролей
может менять собственный пароль и завершать собственные сессии.

## 2. Первый запуск и первый администратор

Из корня backend скопируйте `.env.example` в `.env` и запустите контейнеры:

```sh
cp .env.example .env
docker compose up --build -d
docker compose logs -f backend
```

В PowerShell первая команда — `Copy-Item .env.example .env`.
Дождитесь готовности процесса: `http://127.0.0.1:8080/readyz` должен ответить
`{"status":"ready"}`. Сервер сам применяет миграции, включая новую
`008_access.sql`. Затем в другом терминале выполните:

```sh
docker compose exec backend /app/access init-admin --username oleg --generate-password
```

Команда выводит JSON с карточкой пользователя и `generated_password`. Сохраните
этот пароль в менеджере паролей. Он показывается при создании; backend хранит
только его хеш. Общего пароля вроде `admin/admin` и открытой регистрации нет.
Повторный bootstrap откажет, если активный администратор уже существует.

CLI умеет альтернативный режим `--password-stdin`: пароль читается из
стандартного ввода. Не передавайте пароль аргументом командной строки.
При запуске из Go-исходников сначала запустите `go run ./cmd/server`, затем
в другом терминале с тем же `DATABASE_URL`:

```sh
go run ./cmd/access init-admin --username oleg --generate-password
```

CLI использует уже применённую схему и **не вызывает миграции**: создание
администратора в работающей системе не сбрасывает connected-флаги или сессии
Agent. Если таблицы ещё не созданы, сначала запускайте сервер.

## 3. Вход из браузера: cookie

`POST /api/auth/login` принимает `Content-Type: application/json`:

```json
{"username":"oleg","password":"<ваш пароль>"}
```

Ответ 200 содержит `user`, `expires_at`, `csrf_token`; браузер получает cookie
`infrapulse_session`. Cookie имеет `HttpOnly`, `SameSite=Strict`, `Path=/`;
флаг `Secure` определяется настройкой окружения. JavaScript не должен читать
cookie или копировать токен сессии в localStorage.

После перезагрузки страницы `GET /api/auth/me` возвращает текущего пользователя,
`permissions`, срок сессии и `csrf_token`. Для cookie-запросов, изменяющих данные,
добавляйте **один** заголовок `X-CSRF-Token` с этим значением. При отправленном
`Origin` backend также сверяет его с разрешённым origin. Отсутствие или неверное
значение CSRF у cookie-мутации даёт 403, включая logout и смену пароля.

Минимальный пример для frontend-разработчика, без реализации интерфейса:

```js
const meResponse = await fetch('/api/auth/me', { credentials: 'same-origin' });
if (meResponse.status === 401) throw new Error('Нужно войти');
if (!meResponse.ok) throw new Error(`Backend: ${meResponse.status}`);
const me = await meResponse.json();

const response = await fetch('/api/auth/logout', {
  method: 'POST',
  credentials: 'same-origin',
  headers: { 'X-CSRF-Token': me.csrf_token }
});
if (!response.ok) throw new Error(`Backend: ${response.status}`);
// Ответ logout — 204: JSON-тело читать не нужно.
```

Frontend и API должны быть доступны браузеру через один origin. Для отдельного
порта frontend настройте development proxy `/api` на backend и задайте
`AUTH_PUBLIC_ORIGIN` равным адресу, который видит браузер, например
`http://127.0.0.1:5173`. Проксируйте `/healthz`, `/readyz`, `/openapi.json` при
необходимости. Настройки proxy выполняет frontend-разработчик; CORS в backend
не добавлен. Не смешивайте `localhost` и `127.0.0.1` в одном сценарии входа.

## 4. Скрипты и API-клиенты: Bearer

`POST /api/auth/token` принимает тот же JSON с username/password и возвращает
`access_token`, `token_type: "Bearer"`, `expires_at`, `user`. Cookie этим
маршрутом не устанавливается. Затем клиент отправляет:

```http
Authorization: Bearer <access_token>
```

Это непрозрачный случайный токен, **не JWT**. Его нельзя декодировать, чтобы
получить права: backend ищет SHA-256 токена в PostgreSQL и проверяет срок,
пользователя и актуальную роль. В БД исходный токен не хранится. Токен содержит
256 случайных бит; его нужно защищать так же, как пароль.

Для Bearer не требуется CSRF-заголовок, поскольку браузер не добавляет его
автоматически как cookie. Если `Origin` присутствует у мутации, он всё равно
проверяется. Не передавайте одновременно cookie сессии и Authorization:
неоднозначные учётные данные отклоняются. Вход не принимает Authorization.

## 5. Срок сессии и отзыв доступа

Сессия живёт 8 часов по умолчанию. `AUTH_SESSION_TTL` разрешает диапазон
`1m..24h`. Это абсолютный срок от входа; чтение `/auth/me` не продлевает его.
Refresh-токенов и автоматического продления в этом этапе нет.

На пользователя сохраняются не более 20 сессий. Новый вход удаляет истёкшие
и при необходимости самые старые сессии. Перезапуск backend не отзывает
действующие REST-сессии в БД.

- `POST /api/auth/logout` отзывает текущую сессию и удаляет cookie при её использовании.
- `POST /api/auth/logout-all` отзывает все сессии текущего пользователя.
- Изменение роли, отключение/включение пользователя или смена пароля отзывает
  все его сессии. После изменения доступа нужен новый вход.
- Backend не позволяет отключить или понизить роль последнего активного admin:
  возвращает 409 `last_administrator`.

Уже выполняющийся запрос и последующий запрос — разные операции. Право на
изменение monitoring policy дополнительно проверяется в транзакции сохранения;
завершённое изменение не отменяется последующим выходом пользователя.

## 6. Маршруты управления доступом

| Метод и путь | Кто вызывает | Тело / результат |
|---|---|---|
| `POST /api/auth/login` | Без сессии | username/password → cookie, user, expires_at, csrf_token |
| `POST /api/auth/token` | Без сессии | username/password → Bearer access_token, user, expires_at |
| `GET /api/auth/me` | Любая роль | user, permissions, expires_at, csrf_token |
| `POST /api/auth/logout` | Любая роль | 204, текущая сессия отозвана |
| `POST /api/auth/logout-all` | Любая роль | 204, все собственные сессии отозваны |
| `POST /api/auth/password` | Любая роль | current_password/password → 204 |
| `GET /api/access/users` | admin | items, limit, offset |
| `POST /api/access/users` | admin | username/password/role → 201, user |
| `PATCH /api/access/users/{id}` | admin | role и/или disabled → user |
| `POST /api/access/users/{id}/password` | admin | password → 204 |
| `GET /api/access/audit` | admin | items, limit, offset; новые записи первыми |

Списки users/audit принимают `limit=1..500` (по умолчанию 100) и
`offset=0..1000000` (по умолчанию 0). ID пользователя имеет вид
`usr_` + 32 шестнадцатеричных символа. Возвращаемая карточка содержит `id`,
`username`, `role`, `disabled`, `created_at`, `updated_at`, `last_login_at`.
Password hash и сессионный хеш никогда не входят в карточку.

Логин — 3–64 ASCII символа, начинается с буквы или цифры; разрешены буквы,
цифры, `.`, `_`, `-`. Верхний регистр приводится к нижнему. Пароль — минимум
12 Unicode-символов и максимум 256 UTF-8 байт. Обязательных наборов спецсимволов
нет. Пароль хешируется Argon2id с отдельной случайной солью, 32 MiB памяти,
двумя проходами и параллелизмом 1. Параметры фиксированы; непредусмотренные
значения из повреждённого хеша не исполняются.

Auth JSON ограничен 8192 байтами; неизвестные поля и несколько JSON объектов
отклоняются. Все времена ответов — UTC RFC3339. Подробные схемы — в
`/openapi.json`, версия 0.7.0.

## 7. Журнал действий backend

Таблицы `access_users`, `access_sessions`, `access_audit` добавляет миграция 008.
Журнал включает bootstrap, успешные и неуспешные проверки учётных данных,
создание пользователя, изменение доступа/пароля, logout и изменения monitoring
policy. У записи есть `actor_id`, `target_id`, `resource`, action/outcome, время, request ID
и адрес непосредственного HTTP peer. Изменения доступа/monitoring и их успешные
audit-записи фиксируются в одной транзакции.

`access_audit` отличается от `/api/audit`: последний показывает наблюдения
Linux audit от Agent, первый — действия пользователей backend. Это не полный
журнал всех HTTP-запросов: например, любой отказ CSRF или проверка healthz
не обещаются как отдельная запись. Пароли, cookie и Bearer-токены не пишутся
в эти записи. Peer берётся из сетевого соединения, без доверия к произвольному
`X-Forwarded-For`; за proxy он будет адресом proxy.

Журнал сейчас хранится в той же БД. Retention, отдельный экспорт и защита от
изменения администратором БД ещё не реализованы; неизменяемый audit trail
или внешнюю сертификацию этот этап не заявляет.

## 8. Конфигурация и HTTPS

| Переменная | Значение / смысл |
|---|---|
| `AUTH_MODE` | `required` по умолчанию; `disabled` только для отдельного локального development listener |
| `AUTH_SESSION_TTL` | `8h` по умолчанию, допустимо `1m..24h` |
| `AUTH_PUBLIC_ORIGIN` | Точный внешний origin без пути: схема + host + необязательный порт |
| `AUTH_COOKIE_SECURE` | Для HTTPS — `true`; local Compose явно задаёт `false` |

Local Compose публикует HTTP только на `127.0.0.1` и задаёт
`AUTH_PUBLIC_ORIGIN=http://127.0.0.1:8080`, `AUTH_COOKIE_SECURE=false`.
Если изменили `HTTP_PORT`, поменяйте порт в origin. Для этой локальной настройки
`Secure=false` необходимо, чтобы браузер принимал cookie по HTTP.

Для внешнего стенда используйте HTTPS на reverse proxy, например общий адрес
frontend и API `https://infrapulse.example.org`, и задайте:

```text
AUTH_MODE=required
AUTH_PUBLIC_ORIGIN=https://infrapulse.example.org
AUTH_COOKIE_SECURE=true
```

REST-сервер сам не завершает HTTPS: сертификаты `TLS_CERT_FILE/TLS_KEY_FILE`
относятся к gRPC. Внутренний HTTP listener backend за proxy ограничьте
loopback/private-сетью, недоступной внешним клиентам; внешние клиенты должны
проходить через HTTPS proxy. Передача учётных данных в обход proxy по HTTP
этими переменными не шифруется. Не публикуйте нынешний dev Compose в Интернет
без настройки reverse proxy, credentials БД и сетевых ограничений.

`AUTH_MODE=disabled` разрешён лишь при числовом loopback HTTP listener,
например `127.0.0.1:8080` или `[::1]:8080`; wildcard и имя `localhost`
не проходят проверку. Стандартный Compose слушает внутри контейнера wildcard,
поэтому для него этот обход запрещён. В обходе управления пользователями нет;
это отдельный режим для локальных тестов, а не способ входа admin.

## 9. Ошибки и границы реализации

| HTTP / code | Значение |
|---|---|
| 401 `unauthorized` | Неверные credentials, нет сессии, истёкший/отозванный токен |
| 403 `forbidden` | Текущей роли недостаточно |
| 403 `csrf_failed`, `invalid_origin` | Cookie CSRF или внешний origin не прошёл проверку |
| 409 `username_conflict`, `last_administrator` | Логин занят или изменение оставит систему без активного admin |
| 429 `rate_limited` | Лимит попыток входа или заняты вычислители паролей; есть Retry-After |
| 503 `storage_unavailable`, `auth_unavailable` | Временная недоступность БД/слоя доступа |
| 503 `secure_transport_required` | Не настроен HTTPS или явный локальный HTTP режим |

Ограничитель входа хранит счётчики в памяти одного backend: до 10 попыток
за 5 минут отдельно на username и HTTP peer, общий предел 30 попыток в минуту.
Постоянной блокировки аккаунта нет. За одним reverse proxy peer будет общим;
для большого стенда нужен согласованный proxy rate limit. Это не защита от
всех вариантов распределённого перебора или DoS.
Запрещённые bucket-лимитом запросы не расходуют общий бюджет других пользователей.
Проверки current_password при смене собственного пароля также ограничены до
вычисления хеша, отдельно по ID пользователя и peer. Они используют общий
ограниченный бюджет вычислителей; отказ лимита не запускает ещё один Argon2.

SSO, LDAP/OIDC, MFA, email recovery, per-Agent ACL, multi-tenant scopes и
production PKI/enrollment Agent остаются отдельными задачами. REST-пользователь
и удостоверение Agent — разные механизмы: сессия admin не участвует в gRPC
Hello/ACK и не меняет состояние outbox Agent.

Проектные решения сверены с рекомендациями
[OWASP Password Storage](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html),
[OWASP Session Management](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)
и [OWASP CSRF Prevention](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html).
Их использование не является независимым аудитом безопасности. Реальные
результаты проверок текущей версии фиксируются в [validation.md](validation.md).
