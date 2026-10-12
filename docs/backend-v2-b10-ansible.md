# B10 — отдельный обработчик Ansible и снимки конфигураций

Реализация backend добавляет путь **Asset → разрешённое задание → Linux Automation Worker → Ansible Runner → SSH устройства → зашифрованный snapshot**. Это дополнение к существующему контроллеру, а не замена Agent. Agent и Collector менять не требуется. Фронтенд пока не добавлялся.

**Статус приёмки:** Go-код, PostgreSQL, API, ограничения и контракт Python-адаптера проверяются локальными тестами. Настоящий Ansible и чтение реального сетевого устройства требуют Linux control node, SSH-доступа и проверенного публичного ключа устройства. В текущей Windows-среде нет Docker и установленного WSL. Поэтому реальный запуск на устройстве и сборка нового Docker-образа здесь ещё не подтверждены. Это открытая проверка B10, а не уже состоявшаяся демонстрация.

## 1. Что теперь умеет backend

1. Администратор явно связывает конкретный Asset с IP, SSH-портом, шаблоном, локальной ссылкой на credentials и доверенным публичным ключом устройства.
2. Администратор или operator запускает разрешённое read-only задание. В ответ приходит `202`, идентификатор и состояние `queued`.
3. Отдельный обработчик на Linux забирает задачу из PostgreSQL. Для двух обработчиков используется эксклюзивное получение: одно задание выполняет одна копия.
4. Обработчик ещё раз проверяет актуальную привязку, состояние Asset, роль, активность пользователя и его сессию. Затем выполняет фиксированный Ansible-шаблон.
5. Backend сохраняет конфигурацию и выбранные inventory facts зашифрованными, записывает SHA-256, размер, время получения и привязку к Asset/job/template.
6. Интерфейс может показывать историю задач, безопасные события и список снимков. Полное содержимое снимка доступно только администратору с обязательной записью доступа в аудит.

`succeeded` возникает после успешного Ansible-результата **и** фиксации снимка в PostgreSQL. Отправить задачу и получить успех выполнения — разные действия.

## 2. Разрешённые шаблоны

| template_id | Platform | Чтение конфигурации | Дополнительные сведения |
|---|---|---|---|
| `cisco_ios_read_config_v1` | `cisco.ios.ios` | `show running-config` | `show version`, `ios_facts` с subset `min` |
| `arista_eos_read_config_v1` | `arista.eos.eos` | `show running-config` | `show version`, `eos_facts` с subset `min` |
| `vyos_read_config_v1` | `vyos.vyos.vyos` | `show configuration commands` | `show version`, `vyos_facts` с subset `min` |

Поддерживаемые платформы — перечень реализованных шаблонов, а не обещание работы на всех моделях/версиях прошивок. Конкретную платформу нужно проверить на стенде. Для RouterOS существующий typed command path сохраняется; новый произвольный SSH execution не добавлен.

Read-only означает фиксированные команды чтения и facts-модули. `check_mode` не используется как доказательство безопасности: у модулей чтения он может приводить к пропуску сбора. Устройство желательно предоставить отдельной SSH-учётной записи с правами только на чтение. Терминальные команды подключения могут управлять pager в текущей SSH-сессии; конфигурационные операции в шаблонах отсутствуют.

Версии основных Python-пакетов и collections закреплены в `configs/automation-requirements.txt` и `configs/automation-collections.yml`. Их наличие проверено по официальным реестрам 12.10.2026; запуск комбинации требует Linux-проверки. Транзитивные зависимости пока не зафиксированы отдельным полным lock-файлом. AWX не реализован: выбран Ansible Runner из окончательного плана.

## 3. Что находится в базе

Миграция `032_automation.sql` добавляет:

| Таблица | Назначение |
|---|---|
| `automation_targets` | Текущая проверенная привязка SSH host к Asset с номером ревизии |
| `automation_workers` | Обработчики, результат проверки runtime и время последнего heartbeat |
| `automation_jobs` | Очередь, автор, закреплённые параметры запуска, состояние, дедлайн, отмена и snapshot_id |
| `automation_events` | Коды безопасных событий без stdout, паролей и полной конфигурации |
| `automation_snapshots` | Зашифрованные configuration/facts, SHA-256 и метаданные |

История задания содержит неизменяемую копию привязки на момент создания. Если администратор сменит IP, SSH-ключ, шаблон или credential_ref, старая очередь не начнёт работать по новым параметрам автоматически: её запуск завершается ошибкой проверки ревизии. То же относится к объединённому/выведенному Asset и изменению его ревизии. После таких изменений нужно проверить привязку и создать новое задание.

Для одного Asset одновременно допускается одно `queued`/`running` задание; общая очередь ограничена 100 активными заданиями. Повтор HTTP-запроса с тем же `Idempotency-Key` и содержимым возвращает прежнее задание. Повтор ключа с другим содержимым возвращает `409`.

Обработчик **не вызывает `Store.Migrate`**, не запускает HTTP/gRPC и не изменяет соединения Agent. Миграцию применяет основной backend при запуске.

## 4. Установка обработчика на Linux

Практичный стенд — существующий Docker Compose на Linux или Linux VM, где находятся PostgreSQL и backend. В Windows сам backend продолжает работать; Ansible control node размещается на Linux. Не устанавливайте WSL ради запуска backend, если у команды уже есть Linux VM.

В `.env` укажите:

```dotenv
AUTOMATION_ENABLED=true
AUTOMATION_ENCRYPTION_KEY=<стабильный base64-ключ из 32 случайных байт>
AUTOMATION_WORKER_ID=worker_one
AUTOMATION_ALLOWED_CIDRS=192.0.2.0/24
```

`192.0.2.0/24` — адреса для примера, замените на реальную подсеть лабораторных устройств. Обработчик не принимает DNS-имена устройств: IP явно проверяется по CIDR. Запись `0.0.0.0/0` запрещена. При размещении обработчика отдельно нужна безопасная доступность той же PostgreSQL; стандартный Compose публикует БД только на localhost. Не открывайте её в интернет для соединения с обработчиком.

Сгенерировать ключ на Linux:

```sh
python3 -c 'import base64,secrets; print(base64.b64encode(secrets.token_bytes(32)).decode())'
```

Сохраните его вне GitHub и отдельно от резервной копии БД. Backend и worker используют один и тот же ключ. Потеря или случайная замена ключа лишает доступа к старым снимкам; запуск обнаруживает неправильный ключ. Этот ключ отличается от `DATA_SOURCE_ENCRYPTION_KEY`, хотя используется уже существующая проверенная реализация AES-256-GCM.

Создайте локальный credential-файл `automation-secrets/switch_readonly.json`:

```json
{
  "username": "infrapulse_readonly",
  "password": "<настоящий SSH-пароль>"
}
```

Если устройству необходим `enable`, допускается дополнительное поле `enable_password`. Только эти три поля разрешены. SSH private key auth и jump-host в этом этапе не реализованы. Не добавляйте в файл Ansible-переменные или команды.

На Linux файл должен принадлежать UID/GID `10001`, иметь права `0600`, каталог — доступ для UID `10001`. Например, после создания локальной папки/файла:

```sh
sudo chown -R 10001:10001 ./automation-secrets
sudo chmod 700 ./automation-secrets
sudo chmod 600 ./automation-secrets/switch_readonly.json
```

Эти команды относятся только к специально созданной папке секретов. Docker Desktop на Windows иначе передаёт права bind mounts: проверка приватности файла может отклонить такой файл. Приведённый рецепт предназначен для Linux-стенда.

Папка секретов исключена из Git и Docker build context. Файл монтируется read-only. Пароль не передаётся через REST или командную строку процесса; обработчик отправляет его Python-адаптеру через stdin. Временные inventory/artifacts остаются в закрытом каталоге на время выполнения. В Compose это tmpfs с ограничением размера; после задания каталог удаляется. Исторические Runner artifacts не сохраняются, события в БД содержат только безопасные коды. Administrator машины и привилегированный доступ к памяти процессов остаются частью доверенной границы.

Сначала запустите PostgreSQL/backend, чтобы применить миграцию:

```sh
docker compose -f docker-compose.yml -f docker-compose.automation.yml up -d --build postgres backend
docker compose -f docker-compose.yml -f docker-compose.automation.yml --profile automation up -d --build automation-worker
```

При включённом B9 дополнительно используйте существующий PKI overlay и инструкции B9. Данный пример сам по себе не настраивает PKI для Collector.

Образ обработчика запускается без root, с read-only корневой файловой системой, tmpfs, лимитом памяти/процессов и без дополнительных capabilities. CIDR-проверка относится к адресу устройства; доступ обработчика к PostgreSQL необходим отдельно. Для production дополнительно ограничьте egress сетью и используйте TLS для PostgreSQL.

На старте проверяются реально установленные `ansible-core`, `ansible-runner`, Paramiko, collections и facts/cli modules. При отсутствии runtime `available=false`; создание заданий получает `503`. После установки/смены runtime перезапустите обработчик: проверка пакетов выполняется на старте. Heartbeat каждые 5 секунд подтверждает жизнь обработчика, а не успешный SSH-доступ к каждому устройству. Статус устаревает через 20 секунд.

## 5. Получение доверенного SSH-ключа устройства

В `ssh_host_key` нужны только тип и base64 публичного **host key устройства**, например `ssh-ed25519 AAAA...`. Это не ключ пользователя и не приватный ключ.

Сверьте fingerprint через доверенную консоль/администратора оборудования. `ssh-keyscan` может получить кандидат на ключ, но сам по себе не устанавливает доверие. Автоматическое принятие незнакомых host keys отключено. При изменении ключа обработчик прекращает подключение; администратор проверяет новый ключ и сохраняет новую ревизию привязки.

## 6. Пример API для новичка

Войдите в существующий REST API и получите token обычным способом из инструкции access. Не отправляйте token в чат. В PowerShell:

```powershell
$base = 'http://127.0.0.1:8080'
$headers = @{ Authorization = "Bearer $token" }
$assets = Invoke-RestMethod "$base/api/assets?scope=real" -Headers $headers
Invoke-RestMethod "$base/api/automation/templates" -Headers $headers
Invoke-RestMethod "$base/api/automation/workers" -Headers $headers
```

Выберите существующий реальный Asset устройства. Скопируйте его `id` и `revision`; IP не используется как автоматическое доказательство, что разные источники относятся к одному Asset. Администратор отвечает за проверенную привязку host к Asset.

```powershell
$assetId = '<id Asset>'
$body = @{
  expected_revision = 0
  asset_revision = 1 # замените на текущую revision Asset
  host = '192.0.2.20'
  port = 22
  template_id = 'cisco_ios_read_config_v1'
  credential_ref = 'switch_readonly'
  ssh_host_key = '<проверенный тип и base64 host key>'
  enabled = $true
  reason = 'Проверенная привязка лабораторного коммутатора'
} | ConvertTo-Json
$target = Invoke-RestMethod "$base/api/assets/$assetId/automation-target" `
  -Method Put -Headers $headers -ContentType 'application/json' -Body $body
```

При редактировании передайте `expected_revision = $target.revision`, а не снова `0`. Привязки simulated/merged/retired Assets не допускаются.

```powershell
$jobHeaders = @{ Authorization = "Bearer $token"; 'Idempotency-Key' = 'read-config-demo-001' }
$request = @{ target_revision = $target.revision; timeout_seconds = 60; reason = 'Первый снимок для сравнения' } | ConvertTo-Json
$job = Invoke-RestMethod "$base/api/assets/$assetId/automation/jobs" `
  -Method Post -Headers $jobHeaders -ContentType 'application/json' -Body $request
Invoke-RestMethod "$base/api/automation/jobs/$($job.id)" -Headers $headers
Invoke-RestMethod "$base/api/automation/jobs/$($job.id)/events" -Headers $headers
Invoke-RestMethod "$base/api/assets/$assetId/automation/snapshots" -Headers $headers
```

После `succeeded` и появления `snapshot_id` администратор может прочитать содержимое:

```powershell
$job = Invoke-RestMethod "$base/api/automation/jobs/$($job.id)" -Headers $headers
$snapshot = Invoke-RestMethod "$base/api/automation/snapshots/$($job.snapshot_id)/content" -Headers $headers
# Содержимое может быть чувствительным: выводите только в доверенной среде.
$snapshot.configuration
```

Отмена:

```powershell
Invoke-RestMethod "$base/api/automation/jobs/$($job.id)/cancel" -Method Post -Headers $headers
```

Отмена queued задания окончательная сразу. Для running задания `202` подтверждает запрос отмены, а остановку нужно проверить по конечному состоянию. Обработчик проверяет отмену/права примерно раз в секунду, затем Runner завершает процесс. Уже завершившиеся команды чтения не «откатываются», поскольку конфигурацию они не меняют.

## 7. Права и состояния

| Действие | viewer | operator | admin |
|---|---|---|---|
| Шаблоны, безопасная история задач/снимков | Да | Да | Да |
| Создать read-only задачу | Нет | Да | Да |
| Отменить свою задачу | Нет | Да | Да |
| Отменить чужую задачу | Нет | Нет | Да |
| Настроить привязку / читать workers | Нет | Нет | Да |
| Прочитать полное configuration/facts | Нет | Нет | Да, с аудитом |

Основные конечные состояния: `succeeded`, `failed`, `cancelled`, `timed_out`. При потере обработчика задача получает `failed/worker_lost` после дедлайна + 30 секунд, когда работающий обработчик проверит очередь. Автоматический повтор orphan задачи не выполняется. Если все обработчики выключены, состояние в БД не обновится само; после появления обработчика оно будет сверено.

Timeout задания — 30–600 секунд. SSH connect/command timeout ограничены 15/30 секундами. Configuration ограничена 4 MiB, facts — 128 KiB. БД хранит backend-время получения снимка; это не доказательство синхронизации часов устройства.

## 8. Что будет в следующем этапе B11

Теперь можно получать и хранить фактическую конфигурацию. Следующий слой — baseline/expected configuration, нормализация, diff и drift с привязкой к Asset и сервисам. Изменение конфигурации, произвольные playbooks, автоматическое исправление, AWX, универсальный rollback и scheduled Ansible jobs в B10 не реализованы. Их нельзя считать готовыми по наличию `snapshot_id`.

## 9. Реальная приёмка и источники

Для реальной read-only проверки предусмотрен `TestAutomationRealLinuxRunnerSnapshot`: Linux, `TEST_DATABASE_URL` изолированного стенда, `TEST_AUTOMATION_TARGET_FILE` с reviewed TargetInput, `AUTOMATION_PYTHON`, `AUTOMATION_CREDENTIALS_DIR` и `AUTOMATION_ALLOWED_CIDRS`. Он запускает настоящий Runner, проверяет результат и сохранение зашифрованного снимка в одноразовой PostgreSQL-схеме. Не выводит полученную конфигурацию в журнал. Пока параметры не предоставлены, тест пропускается явно.

```sh
go test ./internal/storage -run '^TestAutomationRealLinuxRunnerSnapshot$' -count=1 -timeout=3m -v
```

Основные решения проверены по официальной документации: [Ansible Runner Python interface](https://docs.ansible.com/projects/runner/en/latest/python_interface/), [network_cli и проверка SSH host keys](https://docs.ansible.com/projects/ansible/latest/collections/ansible/netcommon/network_cli_connection.html), [ios_facts](https://docs.ansible.com/projects/ansible/latest/collections/cisco/ios/ios_facts_module.html), [eos_facts](https://docs.ansible.com/projects/ansible/latest/collections/arista/eos/eos_facts_module.html), [vyos_facts](https://docs.ansible.com/projects/ansible/latest/collections/vyos/vyos/vyos_facts_module.html). Публичные реестры версий: [ansible-core](https://pypi.org/project/ansible-core/), [ansible-runner](https://pypi.org/project/ansible-runner/), [Paramiko](https://pypi.org/project/paramiko/), [Ansible Galaxy](https://galaxy.ansible.com/).
