# B11.1 — эталонная конфигурация и обнаружение отклонений

Этот шаг реализует диагностическую часть B11: **утверждённый эталон → новый снимок Ansible → сравнение → безопасный diff → история**. Сравнение начинается автоматически после успешного сохранения снимка B10. Agent/Collector и frontend не меняются.

Этот документ фиксирует диагностическую границу B11.1. В актуальной сборке B11.2 уже добавляет отдельные policy/request/approval/execution/verification маршруты; см. [B11.2](backend-v2-b11-2-remediation.md). Сами baseline approval и comparison по-прежнему ничего не меняют на оборудовании.

## 1. Что считается эталоном

Администратор выбирает уже полученный успешный снимок B10 того же Asset, проверяет его и явно утверждает через API. Получается immutable baseline revision. Произвольный текст конфигурации или playbook через этот API не принимается.

Из вариантов окончательного плана выбран **утверждённый базовый снимок**. Git repository expected state, загрузка текстовых шаблонов и RouterOS configuration gather ещё не подключены. Это не мешает проверить вертикальный путь B10 snapshot → baseline → drift.

Baseline содержит:

- Asset ID и его ревизию на момент проверки;
- ссылку на snapshot и его read template/platform;
- SHA-256 исходного и нормализованного состояния;
- версию алгоритма нормализации;
- автора, причину утверждения и время;
- возрастающий номер baseline revision.

Сама конфигурация остаётся в зашифрованном snapshot B10. Отдельной незашифрованной копии или полного diff в БД нет. Смена эталона создаёт новую версию; старые результаты остаются в истории.

Подтверждение эталона не доказывает, что администратор правильно выбрал настройки: он отвечает за проверку известного хорошего состояния. Backend проверяет происхождение, права и версии, а не придумывает нужный VLAN.

## 2. Как выполняется сравнение

Когда worker сохраняет очередной успешный снимок, backend в той же PostgreSQL-транзакции:

1. Находит текущий утверждённый baseline этого Asset.
2. Проверяет Asset revision, платформу, источник snapshot и normalizer version.
3. Расшифровывает expected/actual внутри процесса.
4. Нормализует конфигурацию и сравнивает её смысл в поддерживаемых границах.
5. Сохраняет очищенный результат и ссылки на обе версии.

Повтор сохранения/сравнения не создаёт вторую запись для одной пары `(Asset, baseline revision, actual snapshot)`. Без baseline B10 продолжает собирать снимки обычным способом. После утверждения baseline сразу сравнивается самый новый подходящий снимок, даже если он уже был сохранён раньше.

Получение `succeeded` от Ansible само по себе не означает соответствие эталону. B10 говорит «снимок получен», B11.1 — «полученное состояние совпало/разошлось/не может быть уверенно сопоставлено».

## 3. Нормализация: что убирается, а что сохраняется

Версия: `infrapulse_config_v1`.

Для Cisco IOS / Arista EOS:

- CRLF приводится к LF, завершающие пробелы убираются;
- обычные строки комментариев `!` вне banner не влияют на смысл;
- для Cisco игнорируются `Building configuration...`, число байтов в `Current configuration : ... bytes`; завершающий `end` не считается изменением;
- независимые interface blocks сравниваются по имени, поэтому перестановка целых секций не создаёт drift;
- порядок уникальных плоских scalar-команд интерфейса (`description`, access VLAN, switchport mode, shutdown, MTU, speed, duplex) нормализуется;
- ACL, неизвестные команды, вложенные режимы и повторённые scalar-команды сохраняют порядок;
- поддержанный delimiter-style banner сравнивается как непрозрачный текст: `!` и строки, похожие на interface commands внутри banner, не теряются и не становятся командами;
- неполный/неподдержанный banner, дублированные interface sections и процедурные reset/range конструкции дают `unknown`, а не ложное совпадение.

Имена секций ограничены поддержанными типами Cisco/EOS интерфейсов с числовыми номерами. Неизвестный синтаксис не объявляется корректно разобранным. Это ограниченный parser, а не универсальная модель всех команд всех прошивок.

Для VyOS используется консервативное сравнение строк с нормализацией CRLF/концевых пробелов. Порядок `set`/rules/lists сохраняется; семантическая нормализация всей VyOS-конфигурации не заявляется. Для некоторых безвредных перестановок возможен drift, который нужно проверить вручную.

Пределы: конфигурация до 4 MiB, до 50 000 строк, строка до 16 KiB. Неподдержанные/слишком большие данные не получают `compliant`. В API до 200 изменённых контекстов; `total_changes` считает секции global/interface, а не строки полного patch. Если контекстов больше, `truncated=true`, предложение восстановления не появляется.

## 4. Какие данные видит обычный API

Пример безопасного расхождения:

```json
{
  "status": "drift",
  "fresh": true,
  "reason": "differs_from_approved_baseline",
  "latest": {
    "baseline_revision": 1,
    "actual_snapshot_id": "usr_example",
    "status": "drift",
    "changes": [
      {
        "path": "/interfaces/GigabitEthernet0/12",
        "kind": "access_vlan_changed",
        "interface": "GigabitEthernet0/12",
        "expected_vlan": 20,
        "actual_vlan": 30,
        "redacted": false
      }
    ],
    "proposal": {
      "action": "restore_access_vlan",
      "interface": "GigabitEthernet0/12",
      "expected_vlan": 20,
      "actual_vlan": 30,
      "approval_required": true,
      "execution_available": false,
      "limitation": "execution_requires_B11_2_policy_approval_and_live_verification"
    }
  }
}
```

Это сокращённый пример, не полный response. Для неизвестных или чувствительных изменений API возвращает только тип/контекст и `redacted=true`. Пароли, community strings, private keys, banner text и произвольные конфигурационные строки не копируются в shared diff.

Если администратору нужны полные expected/actual для ручного просмотра, он может открыть обе ссылки snapshot через существующий admin `/api/automation/snapshots/{snapshot_id}/content`. Каждое чтение полного содержимого аудируется. Не передавайте его LLM или в общие логи frontend.

## 5. Почему «старый снимок совпал» не означает «сейчас всё хорошо»

`state.status` — текущая оценка, `state.latest.status` — сохранённый результат по конкретному снимку. При следующих ситуациях текущий status становится `unknown`, а история сохраняется:

- нет baseline или подходящего actual snapshot;
- Asset изменился/объединён/выведен после approval;
- SSH target disabled или target revision/platform поменялись после снимка;
- actual snapshot устарел или оказался с будущим временем;
- parser не может безопасно интерпретировать конфигурацию;
- expected/source/normalizer не совпали.

Свежесть задаётся `CONFIGURATION_DRIFT_MAX_AGE=15m`, допустимо `1m..24h`. Это настройка оценки backend, а не команда смены интервала Collector/Ansible. Время snapshot — время получения backend, а не часы устройства.

Повторное сравнение старого снимка сохраняет/возвращает его исторический результат, но текущим остаётся самый новый actual snapshot текущей baseline revision. Поэтому поздний повтор старого `compliant` не закрывает новый drift.

## 6. Контекст VoIP и предложение действия

При drift добавляются ссылки на enabled reviewed VoIP associations этого Asset с `scope=''`. В ответе есть association ID, kind и явное ограничение `reviewed_asset_association_not_proven_port_impact`. URI, IP endpoint и произвольное service name в diff не копируются.

Контекст фиксируется на момент сравнения и не перестраивает всю историю endpoint identity. Это значит «с этим Asset связан проверенный voice service», а не «доказано, что VLAN данного порта обслуживает именно его». Для доказательства нужны port/service mapping, topology и временные сигналы последующего B12. SIP down и автоматическая первопричина в этом шаге не выдумываются.

Предложение `restore_access_vlan` появляется только когда:

1. Есть ровно одно отклонение, и оно состоит только в одной строке access VLAN.
2. В обеих версиях интерфейс явно находится в `switchport mode access` и не является `no switchport`.
3. Имя соответствует поддержанному физическому Cisco/EOS интерфейсу.
4. VLAN в обеих версиях находится в диапазоне 1–4094.

Для trunk, Port-Channel, неполной/неоднозначной конфигурации, других изменений или неизвестного режима предложение не создаётся. **Это рекомендация для review, не исполнение и не разрешение policy.** В актуальной сборке подходящее предложение имеет `execution_available=true`, но без отдельной включённой B11.2 policy и admin approval ничего не запускается.

## 7. REST API и права

OpenAPI: `0.32.0`.

| Маршрут | Право / назначение |
|---|---|
| `GET /api/assets/{id}/configuration/baseline` | admin, текущий approved baseline |
| `PUT /api/assets/{id}/configuration/baseline` | admin, новая версия, explicit confirmation, Idempotency-Key |
| `GET /api/assets/{id}/configuration/baseline/history` | admin, immutable revisions |
| `GET /api/assets/{id}/configuration/drift` | viewer/operator/admin, очищенный текущий статус |
| `GET /api/assets/{id}/configuration/drift/history` | viewer/operator/admin, история сравнений |
| `POST /api/assets/{id}/configuration/compare` | operator/admin, явно выбранный снимок текущей baseline revision |

Mutation проверяет актуальную роль и действующую сессию в PostgreSQL. Старая cached admin role не позволяет утвердить baseline после понижения прав. Есть optimistic concurrency для Asset/baseline и защита повторного ключа. Подтверждение/audit фиксируется вместе с версией в одной транзакции.

У history — `limit=1..200`, `offset=0..100000`. Неизвестные/повторные query-параметры и JSON-поля запрещены. Новые маршруты возвращают `Cache-Control: no-store`. В reasons не помещайте секреты.

## 8. Пошаговый запуск и лабораторный сценарий

### Шаг 1. Обновить backend и worker вместе

Сохраните существующие `.env`, encryption key и credentials вне Git. B11.1 добавляла migration 033; актуальная сборка применяет также migration 034. Worker проверяет наличие migration 034 и не выполняет миграции самостоятельно. Новый binary worker нужно развернуть вместе с backend.

Оставьте настройки B10, при необходимости добавьте:

```dotenv
CONFIGURATION_DRIFT_MAX_AGE=15m
```

На Linux Compose — существующие команды B10. Новый backend/worker образ здесь компилируется как Go binary; реальное Ansible/device выполнение и Docker build остаются отдельной Linux-приёмкой.

### Шаг 2. Получить известную хорошую конфигурацию

Через B10 read-only job получите snapshot лабораторного коммутатора с VLAN 20 на выбранном порту. Убедитесь, что job=`succeeded`, а full snapshot принадлежит именно выбранному Asset и соответствует правильной настройке. Не утверждайте drift-снимок как baseline случайно.

### Шаг 3. Утвердить эталон

PowerShell; `$token` — ваш существующий REST token, его не нужно присылать в чат:

```powershell
$base = 'http://127.0.0.1:8080'
$assetId = '<Asset ID>'
$headers = @{ Authorization = "Bearer $token" }
$asset = Invoke-RestMethod "$base/api/assets/$assetId" -Headers $headers
$snapshots = Invoke-RestMethod "$base/api/assets/$assetId/automation/snapshots" -Headers $headers
$approvedSnapshotId = '<ID проверенного хорошего snapshot>'
$approvalHeaders = @{ Authorization = "Bearer $token"; 'Idempotency-Key' = 'baseline-demo-001' }
$body = @{
  expected_revision = 0
  asset_revision = $asset.revision
  snapshot_id = $approvedSnapshotId
  confirmed = $true
  reason = 'Проверены настройки лабораторного коммутатора'
} | ConvertTo-Json
$baseline = Invoke-RestMethod "$base/api/assets/$assetId/configuration/baseline" `
  -Method Put -Headers $approvalHeaders -ContentType 'application/json' -Body $body
Invoke-RestMethod "$base/api/assets/$assetId/configuration/drift" -Headers $headers
```

Первый baseline использует `expected_revision=0`; при следующем approval передайте фактический текущий baseline revision. Повтор точно такого же запроса с тем же ключом возвращает прежний baseline; изменить содержимое под прежним ключом нельзя.

### Шаг 4. Создать drift на лабораторном устройстве

Команда меняет access VLAN выбранного лабораторного порта на 30 штатным способом управления устройством. B11.1 само это не делает. Затем запустите новый B10 read-only job для того же target revision и дождитесь нового snapshot.

```powershell
$state = Invoke-RestMethod "$base/api/assets/$assetId/configuration/drift" -Headers $headers
$state.status
$state.latest.changes
$state.latest.proposal
Invoke-RestMethod "$base/api/assets/$assetId/configuration/drift/history" -Headers $headers
```

Ожидается `drift`, expected VLAN 20, actual VLAN 30. Если получилось `unknown`, сначала прочитайте `reason`/`latest.error_code`; старое совпадение нельзя выдавать за актуальную проверку.

### Шаг 5. Проверить конкретный снимок повторно

```powershell
$compare = @{ baseline_revision = $baseline.revision; snapshot_id = '<actual snapshot ID>'; reason = 'Повторная проверка evidence' } | ConvertTo-Json
Invoke-RestMethod "$base/api/assets/$assetId/configuration/compare" `
  -Method Post -Headers $headers -ContentType 'application/json' -Body $compare
```

### Шаг 6. Проверить восстановленное состояние

Для B11.1 команда вручную возвращает VLAN 20 в лаборатории и получает ещё один read-only snapshot. Только новый результат сравнения подтверждает `compliant`. Нажатие будущей кнопки восстановления или просто успешный SSH job не заменяет повторную проверку.

## 9. Проверенная часть и следующий шаг

Сценарий 20 → 30 → 20, late replay, concurrency, права, stale/retarget/merged состояния, noise/order/banner и скрытие secret strings проверяются fixtures в PostgreSQL. Они доказывают работу backend-сравнения, а не проведённую демонстрацию на физическом Cisco/Arista/VyOS. Детали: [валидация B11.1](backend-v2-b11-1-validation.md).

B11.2 теперь добавлен отдельным слоем; общий rollback для любого железа по-прежнему не обещается. Полная приёмка B11 требует разрешённого восстановления и реальной проверки на выбранном стенде.
