# B4.1 — подключение Zabbix и inventory

Дата: 11 октября 2026. Изменён только backend. Agent/Collector и frontend не
редактировались, GitHub не обновлялся. Это первая часть B4 окончательного плана:
metrics, problems и events Zabbix ещё не реализованы.

Команда указала целевые версии Zabbix **7.4.15 и 7.0.31**. Они включены
в fixtures этой версии; подключение к установкам команды пока не проверено.
Для следующего B5 зафиксирована указанная командой версия Prometheus **3.15.0**.
Во время этапа обновлена [совместимость backend с Collector Phase 8A](backend-collector-phase8-compatibility.md)
из commit `215d33a`; сам Collector не редактировался.

## Результат

Backend теперь содержит настоящий HTTPS JSON-RPC коннектор, зарегистрированный
при запуске сервера. Для ZABBIX доступны `test` и `sync`, а не только запись
источника в реестре. Используются прежние очередь, scheduler, шифрование токена,
revision checks, idempotency и RBAC; отдельного транспорта для Agent нет.

Поддерживаемый адаптером формат API: стабильные версии 6.0.x, 7.0.x и 7.4.x.
Иные серии и prerelease отвергаются как `unsupported_version`; поддержку будущих
версий нельзя предполагать без проверки. Сначала вызывается `apiinfo.version`
без аутентификации. В 6.0 API token передаётся в `auth` JSON-RPC, в 7.x —
`Authorization: Bearer`. Логин/пароль Zabbix не используются. Официальные источники:
[Zabbix 6.0 API](https://www.zabbix.com/documentation/6.0/en/manual/api),
[Zabbix 7.4 API](https://www.zabbix.com/documentation/7.4/en/manual/api),
[apiinfo.version](https://www.zabbix.com/documentation/7.0/en/manual/api/reference/apiinfo/version).

## Что происходит по шагам

1. Администратор создаёт источник с endpoint, API token и выбранными `host_id`.
   Токен хранится зашифрованным и не возвращается через REST.
2. `POST .../test` создаёт задачу. Worker проверяет TLS, версию API и выполняет
   авторизованный `host.get` с `output=[hostid], limit=1`. Пустой список при
   успешном RPC допустим: это проверка доступа к методу, не наличия оборудования.
   Test не создаёт Assets и не отмечает inventory синхронизированным.
3. `POST .../sync` либо scheduler запускает чтение следующей порции выбранных
   хостов. Нет запроса всего inventory установки и автоматического расширения
   разрешённого списка. Host IDs сортируются численно независимо от порядка API.
4. Результаты преобразуются в общие Assets с bindings `source_instance_id +
   host + host_id`. Два разных Zabbix с одинаковым `host_id` остаются разными
   источниками. Legacy agents/devices не создаются; данные Collector сохранены.
5. Inventory, состояние источника, history, cursor и успешное завершение задачи
   коммитятся вместе. При ошибке, истёкшем lease или изменении revision старая
   порция не публикуется. Изменение настроек сбрасывает cursor.
6. Клиент читает карточки через существующий `/api/assets`, происхождение через
   `/sources`, snapshot через `/inventory`, версии через `/facts`.

## Какие сведения сохраняются

Нормализованный Device: `hostid`, отображаемое имя, техническое имя, vendor,
model, основной serial, OS, architecture, software, location и management address.
Идентификатор хоста Zabbix не превращается в hardware UUID. Тип устройства
задаётся администратором; по умолчанию `unknown`. IP/имя не определяют тип.
Для смешанного списка хостов пока нужно оставить `unknown` либо создать
отдельные источники с разными selections; настройки per-host ещё нет.

`provider_state` хранит `api_version`, `status`, `monitoring_enabled`,
`maintenance_status`, `maintenance_type`, `active_available` (7.x), interfaces,
groups и tags. В 6.0 запрос использует `selectGroups`, в 7.x `selectHostGroups`.
Нужные поля выбираются явно, без `extend`, macros, TLS PSK, interface errors и
SNMP credentials. См. [host.get 6.0](https://www.zabbix.com/documentation/6.0/en/manual/api/reference/host/get),
[host.get 7.0](https://www.zabbix.com/documentation/7.0/en/manual/api/reference/host/get).

`status=0` означает включённый мониторинг, `1` — отключённый. Interface
availability и active agent availability — отдельные факты мониторинга.
Их нельзя подменять физическим `online`, power state или hardware health.
Эта часть адаптера их не вычисляет. Источник:
[host object](https://www.zabbix.com/documentation/7.0/en/manual/api/reference/host/object).

Версия фактов добавляется только при изменении inventory или `provider_state`.
Повторное чтение той же порции обновляет свежесть, не дублирует историю.
Времена `observed_at/collected_at` — время чтения backend, не время измерения
Zabbix. Source timestamp отсутствует, clock quality `unknown`.

Serial из пользовательского inventory по умолчанию имеет confidence `medium`.
`verified_inventory=true` — явное подтверждение администратора, что serials
проверены; только тогда typed serial получает `high`. Это не криптографическое
доказательство уникальности. Automatic resolver по умолчанию выключен. При
включении продолжают действовать консервативные правила B3.3: известный kind,
подтверждающие vendor/model, свежесть, отсутствие конфликтов и единственный
кандидат. IP/hostname сами по себе никогда не объединяют Assets.

Миграция 023 добавляет JSONB `provider_state` в bindings и fact versions.
Для предыдущих Collector записей это `{}`; старые snapshots не переписываются.
OpenAPI обновлён до 0.24.0, старые REST маршруты и права сохранены.

## Настройка стенда

1. В Zabbix создайте отдельного пользователя/роль с доступом только на чтение
   выбранных host groups и с разрешением API methods `apiinfo.version`/`host.get`.
   Создайте API token; права на изменения не нужны. Не передавайте токен в чат,
   README, GitHub или query URL. Токен без доступа к `host.get` получит ошибку.
2. В окружении backend установите стабильный `DATA_SOURCE_ENCRYPTION_KEY`
   (base64 32 random bytes). Сохраните исходный ключ: без него токены из БД
   не расшифруются. Способ генерации описан в B2.1.
3. Разрешите сервер в сетевой политике, например:

```dotenv
DATA_SOURCE_ALLOWED_ORIGINS=https://zabbix.example:443
DATA_SOURCE_ALLOWED_CIDRS=10.20.0.15/32
DATA_SOURCE_ALLOW_LOOPBACK=false
DATA_SOURCE_CA_FILES={"lab":"/run/source-ca/lab.pem"}
```

Это пример, адреса замените своими. Если сертификат доверен системой, уберите
mapping CA и `tls_ca_ref`. Для private CA нужен публичный CA PEM, смонтированный
read-only в Docker; leaf certificate/private key не подходят. Одного HTTPS
origin недостаточно: все DNS IP должны попадать в разрешённые CIDRs.
Подробнее — [B2.3](backend-v2-b2-network.md). После изменения окружения
перезапустите backend, а не Agent. Прямой HTTP Zabbix в этой версии не поддержан:
для лаборатории поднимите HTTPS перед его frontend с проверяемым сертификатом.
4. Получите backend admin Bearer session через `/api/auth/token`, как в
[инструкции доступа](access-stage5.md). Zabbix token и backend session — разные
секреты. В примере ниже `$backendToken` — уже полученная backend session.
5. Создайте источник из PowerShell; реальные secrets не сохраняются в файле:

```powershell
$base = 'http://127.0.0.1:8080'
$headers = @{ Authorization = "Bearer $backendToken" }
$zabbixSecret = Read-Host 'Zabbix API token' -AsSecureString
$body = @{
  type = 'ZABBIX'
  name = 'Zabbix lab switches'
  endpoint = 'https://zabbix.example/api_jsonrpc.php'
  enabled = $true
  sync_interval_seconds = 30
  credential = [System.Net.NetworkCredential]::new('', $zabbixSecret).Password
  configuration = @{
    timeout_seconds = 15
    tls_ca_ref = 'lab'
    zabbix = @{
      host_ids = @('10601', '10602')
      page_size = 20
      device_type = 'switch'
      verified_inventory = $false
    }
  }
} | ConvertTo-Json -Depth 6
$source = Invoke-RestMethod -Method Post -Uri "$base/api/data-sources" `
  -Headers $headers -ContentType 'application/json' -Body $body
Remove-Variable body, zabbixSecret
```

Host IDs — именно внутренние ID Zabbix, не IP и не hostname. Пустой/отсутствующий
список допускается для `test`, но `sync` завершится `configuration_required`.
Следите за JSON body limit 8 KiB; credential API token должен быть 16..16384
ASCII символов Bearer alphabet (обычный token Zabbix удовлетворяет этому).
6. Запустите задачи и проверьте состояние/Assets:

```powershell
$jobHeaders = @{ Authorization = "Bearer $backendToken"; 'Idempotency-Key' = 'lab-zabbix-test-1' }
$job = Invoke-RestMethod -Method Post -Uri "$base/api/data-sources/$($source.id)/test" -Headers $jobHeaders
Invoke-RestMethod -Uri "$base/api/data-sources/$($source.id)/jobs/$($job.id)" -Headers $headers
$jobHeaders['Idempotency-Key'] = 'lab-zabbix-sync-1'
$job = Invoke-RestMethod -Method Post -Uri "$base/api/data-sources/$($source.id)/sync" -Headers $jobHeaders
Invoke-RestMethod -Uri "$base/api/data-sources/$($source.id)/jobs/$($job.id)" -Headers $headers
Invoke-RestMethod -Uri "$base/api/assets?scope=real&limit=50" -Headers $headers
```

202 означает постановку в очередь. Дождитесь `succeeded`; `running`,
`retry_wait`, `failed` и `cancelled` не означают готовый inventory. Scheduler
может уже запустить sync для enabled источника: в этом случае 409 `source_busy`
означает, что нужно дождаться текущей задачи. Операции/настройки требуют admin;
карточки/историю читают viewer/operator/admin. Cookie-мутации требуют CSRF.
Редактирование — `PUT /api/data-sources/{id}` с последним `revision`; credential
можно не передавать, если endpoint не меняется.

## Порции, свежесть и ошибки

Selection: до 200 уникальных host IDs, положительный uint64 без ведущих нулей.
Page size: 1..50, default 20. Interval: 3..86400 секунд **между порциями**,
отсчитывается после commit. Для 200 хостов, page 20, interval 3s полный цикл
занимает примерно 30 секунд плюс HTTP/очередь; интервал не ускоряет сбор Zabbix.
Для больших installations следующий шаг — настраиваемая scope selection и
полный цикл отдельно от интервала страниц. Пока ограничение явное и проверяемое.

Если выбранный host исчез/недоступен по правам, весь текущий page получает
`selected_hosts_unavailable`, cursor остаётся прежним; Assets не удаляются.
Исправьте permissions или selection. Это не автоматическое обнаружение удалённых
хостов и не retirement. Обновление settings запускает цикл заново.

Фиксированные error codes: `invalid_credentials` (пустой/невалидный token либо
HTTP 401/403), `api_rejected` (JSON-RPC отказ, включая некоторые ошибки token/
permissions), `unsupported_version`, `configuration_required`,
`selected_hosts_unavailable`, `invalid_response`. Тело RPC error никогда не
попадает в logs/job events. Timeout/429/5xx имеют ограниченный retry/backoff;
lease не допускает публикацию старого результата. Source `connected` показывает
успех последнего API обращения, а не постоянно проверяемую связь или online хоста.

Размер ответа ≤4 MiB; максимум 128 interfaces/groups/tags на host, bounded
strings и object provider state ≤256 KiB. Слишком большая/невалидная порция
не считается успешной. Tokens/macros/PSK специально не запрашиваются. В tags
маскируются известные названия секретов, Bearer/Basic, private keys и эхо токена
этого source; произвольные secrets в пользовательском inventory/tags невозможно
распознать гарантированно. Выбранные поля доступны всем data.read пользователям
этой установки — заранее проверьте состав inventory и tags команды.

## Проверка и границы готовности

Unit tests: formats 6.0/7.0/7.4, unauthenticated version, разные способы token,
read-only whitelist, selection/order/page wrap, отсутствующие/чужие/duplicate
hosts, mismatched RPC ID, error redaction, HTTP 401/429/503, invalid cursor/config.

PostgreSQL integration: реальный защищённый HTTPS transport к локальному
JSON-RPC fixture → очередь → encrypted credential → inventory/Assets/history.
Проверены source isolation, unchanged facts, изменения provider status, cursor,
отмена после revision change и отсутствие выдуманного Agent. Общие tests также
проверяют прежние Agent/Collector process scenarios, migrations, REST/RBAC и ACK.

Fixture моделирует Zabbix API; это **не приёмка на настоящем Zabbix команды**.
Для live приёмки нужны версия/HTTPS endpoint/permissions и выбранные host IDs;
токен вводится локально. Docker/race/Linux-only process checks недоступны здесь.
B4 целиком ещё не закрыт: следующий B4.2 — mapping items/metrics,
problems/events, их история/свежесть и проверка на реальном стенде.
