# B12.3a — явные hardware-инциденты

Backend превращает сохранённые состояния оборудования в инциденты и normalized signals категории hardware. Agent/Collector, protobuf и frontend не изменялись.

## Как проходит сообщение

1. Collector присылает inventory/component/HardwareHealth с явными Health/HealthRollup. Уже существующая B8 проекция проверяет namespace источника и привязку к Asset, сохраняет неизменяемый sample и latest.
2. В той же транзакции, что и сообщение, sample попадает в hardware_signal_pending. gRPC ACK не ожидает вычисления правил/корреляции.
3. Существующий correlation worker каждые 3 секунды сначала обрабатывает до 25 hardware samples, затем до 25 incident projections. При нагрузке это интервал проверки очереди, а не гарантия доставки каждого инцидента за 3 секунды. Общий проход ограничен 20 секундами.
4. hardware.vendor_health v1 создаёт или обновляет обычный incident с resource_kind=hardware. Resource ID — observation_key, context — точный binding ID. Fingerprint изолирует agent/device/source/simulation scope/resource/context.
5. Обычная durable очередь корреляции строит normalized signal, связывает его с Asset, сервисами и другими сигналами по существующим правилам B12.1. Связь с Asset требует именно исходный binding ID; похожее имя другого устройства не используется.

## Правило и восстановление

- Явный warning/critical в Health или HealthRollup немедленно открывает проблему. Если присутствуют оба, выбирается максимальная серьёзность.
- OK признаётся только при наличии хотя бы одного явного ok и отсутствии неизвестного непустого значения во втором поле. Регистр и пробелы нормализуются.
- Восстановление требует трёх последовательных свежих OK с различными возрастающими observed_at. Их sample IDs, даты и состояния сохранены в evidence. Подтверждённая новая сессия Collector сбрасывает серию.
- При наличии source_timestamp он тоже должен продвигаться. Повторное чтение одного timestamp не считается отдельным подтверждением; возврат timestamp назад не сбрасывает его сохранённый максимум.
- Unknown, плохие часы, разрыв больше freshness interval, недоступный или отключённый источник сбрасывают серию. Отсутствие сообщений не закрывает incident. Для чтения его актуальности используются latest, source enabled, Collector connection/session, clock/data quality и source timestamp.
- Старые и равные observed_at не переписывают уже обработанное состояние; observed_at далеко в будущем не отравляет курсор обработки.
- Для восстановления используется freshness B8: минимум 300 секунд; при подтверждённом collector_applied interval окно растёт до трёх соответствующих inventory/telemetry интервалов. Это freshness window, а не правило на численные температуры.
- Clock quality, отличное от synced, не даёт точную временную уверенность при корреляции. Неизвестное текущее vendor health отображается как evidence_status=unknown, сохраняя незакрытую проблему.

PowerState=Off, RPM=0, отсутствующий sensor и неизвестные единицы сами по себе не означают неисправность. Численные temperature/fan/voltage thresholds требуют отдельной политики по оборудованию; этот этап не подменяет её универсальным порогом. Metric records остаются доступными в B8 API; данное правило обрабатывает inventory/component/health records. IPMI без явного vendor health остаётся unknown.

## Что получит frontend

Действуют существующие роли/auth и API:

```text
GET /api/incidents?resource_kind=hardware
GET /api/incidents?rule_id=hardware.vendor_health
GET /api/incidents/{id}
GET /api/correlation/incidents
GET /api/correlation/incidents/{id}
GET /api/reports/summary?resource_kind=hardware
GET /api/assets/{id}/hardware
```

Incident signal.hardware содержит sample_id, observation_key, binding_id, resource_id, subject_id, canonical health/health_rollup, clock_quality, freshness и recovery_count. Если текущая inventory привязка уже описывает другой target/protocol/instance, старое наблюдение не участвует в новом правиле и не считается текущим доказательством.

Нестандартный vendor health сохраняется в incident только как unknown; raw envelope не копируется в доказательство. Resource evidence с record_kind=hardware ссылается на sample ID, а не telemetry ID. OpenAPI 0.37.0 описывает дополнительный тип, старые типы сохранены.

## Обновление и ограничения

Migration 038 применяется автоматически и не удаляет старые инциденты/telemetry. В очередь однократно попадают текущие non-metric latest записи. Исторический архив полностью не переигрывается. Samples, cursor, incident, evidence и очередь корреляции обновляются атомарно: при SQL failure обработчик повторяет страницу после отката.

Страница hardware обрабатывается раньше страницы correlation; ошибка hardware откладывает данный проход корреляции и видна в worker log. Очередь сохранена для повторного запуска. Метрики backlog и отдельный worker могут понадобиться для большой нагрузки.

Это завершение hardware vendor-status producer, а не всего B12. Отдельные log/drift/topology producers и проверка на реальном стенде ещё остаются. Автоматических команд по hardware health нет.
