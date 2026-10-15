# B12.3b — reported log events в общих инцидентах

Backend добавляет producer для явно высокой severity в логах. Agent/Collector, protobuf и frontend не менялись. Существующие поиск, audit API, raw-права и хранение оригинальных записей сохраняются.

## Путь записи

1. B9/logbook принимает и сохраняет LogBatch, защищает record identity от replay и конфликта содержания, маскирует публичный текст. Высокая severity нового сообщения попадает в log_signal_pending в той же транзакции. Это один SQL insert-select на batch, а не сложный анализ до gRPC ACK.
2. Worker раз в 3 секунды обрабатывает до 25 hardware samples, до 25 log records и до 25 incident projections в рамках общего 20-секундного timeout. При большой очереди 3 секунды — частота проверки, а не гарантия задержки для каждого события.
3. Правило log.reported_severity v1 принимает severity 0–2 как critical и 3 как warning. Severity 0 сохраняет своё значение; nil, 4–7 и неизвестные значения не создают инцидент этим правилом. Слова «error» или «failed» в произвольном сообщении сами по себе не используются.
4. Привязка — существующий inventory binding того же Collector, device ID и точного simulation scope. Placeholder из одного лога, claimed hostname, peer IP, TLS identity или произвольный SourceRef не создают Asset и не выбирают чужое устройство. Source-owned device reference не является аутентификацией физического syslog sender.
5. Записи одного binding/scope/source/channel/process объединяются в фиксированное UTC-окно 5 минут. Другие процессы, источники и окна имеют отдельные resource IDs. Внутри группы severity не снижается, event_count учитывает уникальные сохранённые записи.
6. Обычный incident resource_kind=log преобразуется в normalized signal категории log и участвует в B12.1 корреляции с Asset/service/site/topology/другими инцидентами. Каузальная связь автоматически не утверждается.

## Время и потеря связи

Квалификация относится к моменту приёма: event_at и observed_at не должны отставать от received_at более чем на 300 секунд или опережать его более чем на 30 секунд. Старый original_timestamp не становится свежим при перечитывании файла. Future/invalid timestamps и degraded/unsynced clock не используются; source_timestamp проверяется относительно момента приёма.

Задержка worker после принятого сообщения не уничтожает событие. Если источник уже отключился, принятая запись всё равно создаёт reported-event incident, но evidence_status показывает source_unavailable. Возраст старше 300 секунд даёт stale, а не recovery. При выключенном администратором source, отсутствующем/conflicting Asset binding или прежнем simulation session новое событие этим producer не создаётся; сам log остаётся в архиве.

При неизвестном качестве часов время корреляции estimated. Для original_timestamp оно тоже estimated: synced clock Collector не доказывает синхронизацию часов отправителя syslog.

## Доказательства

signal.log содержит record_id, binding_id, SHA-256 source_id_hash/process_hash, ограниченное маскированное имя процесса, severity исходной записи, event_at/received_at, window_start, event_count, time_quality и lifecycle. Исходный channel ID не копируется в incident. Внутри resource_evidence сохранены первая и последние записи, максимум 20. Все привязки доступны через отдельный paginated endpoint; сумма event_count не ограничивается этим preview.

Raw/message/hostname/peer/TLS metadata не копируются в новое доказательство. При необходимости оператор открывает существующий безопасный Asset log API по log ID; privileged raw по-прежнему требует своих прав. Query raw=true у нового endpoint ничего не раскрывает.

## Проверка оператором

Это reported event, а не доказательство текущей физической поломки. Обычный acknowledge остаётся отметкой «принято в работу». Новый review закрывает только обработанную группу логов, сохраняя исходные severity и доказательства. Тишина, INFO/OK запись и отключение источника сами по себе её не закрывают.

```http
POST /api/incidents/{id}/review-log-event
Content-Type: application/json
Authorization: Bearer <session token>

{
  "confirmed": true,
  "expected_event_count": 3,
  "reason": "Проверили зарегистрированные события; результат отражён в журнале"
}
```

Права — operator/admin с повторной проверкой живой session/role в БД. Cookie writes требуют обычный CSRF. Тело закрыто: неизвестные поля не принимаются. Reason обязателен, 5–512 байт; сохраняется после маскирования. expected_event_count должен совпасть с прочитанным signal.log.event_count.

Если в том же окне есть ещё непрочитанные worker записи, review отвечает 409: дождаться обработки, перечитать incident и проверить новые события. Agent row lock сериализует review с приёмом очередного сообщения. Уже принятый новый record попадает в pending/count conflict; сообщение после завершённого review создаёт новую occurrence вместо скрытого закрытия. Повтор уже успешного review с тем же count возвращает прежний результат без второго audit.

После review state=resolved означает именно «reported event проверен», signal.log.lifecycle=reviewed_event. Поля reviewed_at/by/reason и transition operator_reviewed_log_event поясняют смысл. RecoveryEligible остаётся false. Закрытие связанной CPU/hardware/SIP проблемы не происходит; общий case закрывается лишь после отдельного завершения всех members. Команды устройствам не отправляются.

Для frontend подпись действия должна отражать проверку события, например «Событие проверено». Нельзя трактовать её как подтверждение физического восстановления.

## API и обновление

```text
GET /api/incidents?resource_kind=log
GET /api/incidents?rule_id=log.reported_severity
GET /api/incidents/{id}/log-records?limit=100&offset=0
POST /api/incidents/{id}/review-log-event
GET /api/correlation/incidents
GET /api/reports/summary?resource_kind=log
```

Proof page: 1–200 records, offset 0–100000. Действуют существующие DataRead права. OpenAPI 0.38.0 включает новый kind, evidence и endpoints; старые маршруты сохранены. /api/health/rules перечисляет log.reported_severity и hardware.vendor_health, но общие CPU sustain/trend параметры не являются параметрами этих правил.

Migration 039 применяется автоматически, добавляет queue, source-record links и batch index, не удаляет прежние таблицы/данные. При обновлении однократно в очередь попадают максимум 10000 последних severe records за 5 минут. Полного исторического переигрывания нет. Explicit old inbox backfill сохраняет старые логи для поиска/отчётов, без автоматического превращения всего старого архива в текущие инциденты.

При SQL failure вся worker page откатывается: log, pending и предыдущий incident остаются целыми; следующий проход повторяет обработку. Невозможность durable queue write откатывает ingestion до ACK. Ошибка producer видна в worker log и откладывает текущий проход корреляции.

B12 ещё открыт: нужны отдельные правила для log/audit без severity, configuration drift/topology producers и общая приёмка на командном стенде. Этот этап не объявляет arbitrary log text AI-classified, security breach proven или hardware recovered.
