# B8.1 — hardware по Asset

## Модель и сохранение

`internal/assethardware` извлекает нормализованные inventory/component/health/metric observations из уже поддерживаемого protobuf. `asset_hardware_samples` хранит историю; `asset_hardware_latest` — ссылку на текущее значение. Запись идёт в той же транзакции, что inbox, legacy telemetry и Asset Registry. Ошибка hardware projection отменяет всю транзакцию; Collector может повторить сообщение.

Идентичность наблюдения включает binding, namespace SourceRef, resource/subject ID, kind, name и unit. Сенсоры разных источников Redfish/IPMI сохраняются отдельно. Одинаковый sensor name, число или URI не доказывает физическое тождество. Перед привязкой проверяются Collector, target/type/protocol/instance и scope инвентаризации; чужая target observation остаётся в archive без угадывания владельца.

Latest выбирается по времени наблюдения. Поздняя доставка старого значения не перезаписывает новое. Будущее наблюдение за пределами допустимого clock skew хранится в истории, но не становится latest. Повтор agent/message/sample_index не дублируется. Внешние ключи связывают samples/latest с правильным binding/source.

Hardware inventory содержит тип ресурса, мощность/питание, status, firmware/software, serial/model/vendor, identity evidence и исходную нормализованную hardware payload. Components и telemetry сохраняют свои source/time. Отсутствующие optional-поля protobuf остаются отсутствующими внутри payload; metric value=0 — измеренный ноль. На верхнем уровне power_state, health, health_rollup и operational_state — отдельные поля.

## Чтение

```text
GET /api/assets/{id}/hardware?kind=metric&limit=50
GET /api/assets/{id}/hardware?include_related=true&max_age_seconds=300
GET /api/assets/{id}/hardware/history?after_id=0&limit=50
GET /api/assets/{id}/hardware/history?observation_key=<64 hex>&after_id=123
```

Все запросы требуют data.read. Latest: kind inventory/component/health/metric, offset до 100000. History: курсор after_id возрастает по порядку сохранения; next_after_id использовать вместе с has_more. Общие фильтры source_instance_id, include_related (по умолчанию true), limit 1–200 (по умолчанию 50).

max_age_seconds 3–86400, по умолчанию 300: это порог свежести представления, не команда менять расписание Collector и не политика открытия инцидентов. Frontend может выбрать порог под частоту аппаратного модуля. Долгое расписание не должно ошибочно показываться как мгновенные данные.

Freshness: fresh, stale, source_stale, clock_skew, clock_degraded, source_unavailable либо явно переданное качество данных. Проверяется source.enabled и реальная активность Collector (connected плюс last_seen/timeout), для симуляции — текущая сессия. Hardware health не подменяется этим признаком: stale reading не значит Critical hardware.

Каждое item содержит настоящий asset_id владельца, binding/source_instance_id/source_name, scope, message_id, resource/subject, source_ref, observation_time, observed/collected/source/received times, value/unit/status и payload. Размер ответа ограничен 8 MiB; неполная выдача обозначена has_more.

## System и BMC

include_related использует только разрешённые management relations B7. Manager/BMC и System остаются разными Assets; relation_ids и related=true показывают, почему дополнительная запись присутствует в карточке. Нет выдуманного физического кабеля и переноса sensor identity в System. При усечённом inventory отношений связанные данные не угадываются, relations_truncated=true.

## Обновление и проверки

Миграция 028 seed переносит текущие inventory facts и последнее подходящее hardware telemetry на binding, страницами по 100. Сохраняется исходное received_at из inbox. Старые/future/неподходящие target telemetry не выбираются как текущие. Seed и повтор миграций идемпотентны; полный архив не сканируется ради копии всей истории.

Тесты проверяют 0 и Off, source/target isolation, chronology/skew, verified BMC relations, merge/split, rollback при ошибке projection, seed и freshness просроченного соединения. Process test дополнительно запускает готовый Collector 215d33a против Redfish fixture, принимает реальные gRPC сообщения процесса и читает новые authenticated hardware API.
