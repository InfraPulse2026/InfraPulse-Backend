# B15 — интерфейс метрик и оценка хранения

Это этап подготовки к масштабированию, P2 по окончательному плану. PostgreSQL остаётся рабочим хранилищем. Отдельная TSDB и Grafana не являются обязательными зависимостями и автоматически не устанавливаются.

## Реализация

`internal/storage/metric_store.go` содержит интерфейс MetricStore: Write, Latest, QueryRange. Он адаптирован к существующим типам Telemetry и protobuf envelope, а не вводит второй несовместимый формат CPU/RAM. Store реализует этот интерфейс; compile-time assertion защищает контракт.

Write принимает только telemetry envelope и использует существующий Ingest: в одной транзакции сохраняются receipt, все samples и связанные проекции; ACK разрешён после commit. Повтор не добавляет samples, конфликт payload не перезаписывает прошлый факт. Метод не создаёт второго endpoint для записи в обход Collector validation. Controller продолжает использовать прежний Ingest, который принимает остальные типы сообщений тоже.

Latest требует пару agent_id/device_id и возвращает последнее наблюдение. QueryRange требует эту же пару, использует полуоткрытый период `[from,to)`, максимум 31 день и 1000 samples. Результат отсортирован по observed_at/id, truncated явно показывает превышение лимита. Бюджет чтения — 10 секунд. Пропуски и неизвестные значения не превращаются в нули; uint64 memory остаётся строкой, исходный Sample JSON сохраняется. Это внутренний контракт хранения; существующие REST metrics/history не меняют JSON и права доступа. Нового публичного маршрута для range на этом этапе нет.

Для Prometheus уже есть отдельный query adapter B5: persisted mapping/target/source IDs → ограниченный scoped PromQL → query/query_range. Он читает историю на самом источнике, сохраняет quality и time_semantics=promql_evaluation. Не следует выдавать evaluation timestamp за исходное время Collector sample или копировать все внешние series в PostgreSQL. Существующие `/api/assets/{id}/prometheus/...` маршруты и их ограничения сохранены.

## Синтетическое измерение

Opt-in тест `TestMetricStoreSyntheticLoad` создаёт отдельную временную схему через integrationStore и удаляет её после выполнения. Требуется expendable TEST_DATABASE_URL и INFRAPULSE_METRIC_LOAD=1. Пример запуска: `go test ./internal/storage -run '^TestMetricStore' -count=1 -v`. Без переменной load-тест пропускается; без test DB интеграционные тесты тоже пропускаются, это не считается проверкой PostgreSQL.

Набор: 20 devices, 100 envelope batches, 2000 samples CPU/RAM с временной сеткой 3 секунды, 100 range reads по 100 samples. Измеряются фактическая скорость записи, p50/p95 времени транзакции envelope и чтения диапазона, размер telemetry с индексами, размер agent_messages с индексами, distinct native agent/device/source channels. Эти channels **не являются полным числом scalar/interface series Prometheus**. Набор не включает все counters, topology, logs, оборудование и concurrent frontend users.

Первый прогон PostgreSQL 18 на локальном Windows одновременно с общей интеграционной проверкой: 191.5 samples/s, envelope p50 103.86 ms/p95 143.92 ms; range p50 0.557 ms/p95 1.107 ms. Telemetry relation 1,523,712 bytes, envelopes relation 131,072 bytes. Это наблюдение для маленького синтетического набора под тестовой конкуренцией, **не production SLA и не доказательство ёмкости реального стенда**.

При 3-секундной сетке одно устройство создаёт 28,800 samples в сутки. Для 20 устройств это 576,000 в сутки и 17,280,000 за 30 дней. Линейная оценка только telemetry по первому маленькому набору — около 13.16 GB за 30 дней. Она не учитывает WAL, backups, bloat/vacuum, envelope duplication и остальные таблицы, а fixed-page overhead делает экстраполяцию неточной. Actual active series и уникальные labels внешнего Prometheus следует измерить на его сервере; лимиты выбираемых targets/mappings backend не заменяют это измерение.

## Решение и следующий нагрузочный стенд

Для демонстрации сохраняем PostgreSQL и его индекс agent/device/observed_at/id. Автоматического удаления исторических samples/receipts и миграции в TSDB нет: retention должен учитывать replay и ссылки на evidence. Перед переходом измерить несколько размеров набора, длительный steady load, p95/p99 при чтении API, CPU/RAM/IO PostgreSQL, WAL/backups и фактическую внешнюю cardinality. Маленький набор выше достаточен для повторяемого baseline инструмента, но недостаточен для решения о production TSDB.

Будущий TSDB adapter не может самостоятельно писать samples и сразу подтверждать envelope: нужен PostgreSQL transactional outbox с устойчивым delivery ID, идемпотентным экспортом, recovery worker и определённой политикой ACK. Ledger, Asset registry и incident evidence остаются в PostgreSQL. Prometheus history можно продолжать читать на источнике. Новое хранилище должно проходить существующие replay/zero/null/uint64/isolation проверки.

Grafana остаётся опциональной. До появления согласованных dashboard UID, datasource UID и variable names backend не генерирует выдуманные ссылки. При подключении переменные должны содержать namespace source_instance_id + target/mapping либо agent/device; Asset ID сам по себе не является именем series. Ссылки не должны содержать credentials. Для MVP графики доступны через существующие JSON API, frontend выполняет другой разработчик.
