# B12.5a — события смены объявленного соседа

Backend сравнивает два полных свежих снимка одного устройства и источника. Если на том же локальном порту LLDP/CDP объявлен другой typed chassis/remote-port identity, создаётся warning topology.peer_changed. Это наблюдаемое изменение объявления, а не доказательство замены кабеля, атаки, отказа или причины SIP/CPU-сбоя. Источник может сообщить изменение имени в chassis subtype: физическую идентичность оборудования из одного такого объявления доказать нельзя.

## Границы сравнения

Первый снимок устанавливает основу сравнения без предупреждения. Обычное обновление timestamp, имени/описания соседа и метаданных не меняет peer hash. Нужны COMPLETE, high confidence, LLDP/CDP, непустые typed local-port/chassis/remote-port IDs, свежесть на момент получения и приемлемые часы. MAC chassis subtype требует шесть байт. Локальный порт определяется protocol + subtype/bytes и контекстом source/target/profile; смена target/profile начинает новый контекст.

Partial, failed, unspecified, MNDP, неоднозначные несколько соседей на одном порту, невалидные часы/IDs не меняют основу для этого правила. Остальные данные остаются в прежнем архиве и API карты. Лимит incident snapshot — 512 observations и 1 MiB JSON; больший снимок не становится baseline этого правила, но общий topology модуль продолжает принимать его в прежних пределах.

Если порт отсутствует в одном из двух полных снимков, сравнение не объявляет отказ. Появившийся впервые порт тоже не создаёт warning. Если TTL предыдущего наблюдения закончился до следующего, непрерывность не доказана: начинается новая основа. TTL наследует существующий Prepare: явный expiry/TTL либо его документированный default. Тишина и потеря Collector не закрывают событие. Отдельные disappearance/physical-link failure rules в B12.5a не реализованы.

## Хранение и инциденты

Migration 042 добавляет topology_signal_pending, topology_signal_state и topology_incident_records. Snapshot и очередь сохраняются в одной транзакции с envelope. Worker обрабатывает до 25 снимков в порядке observed_at/id, вне gRPC ACK. Поздние/одинаковые observed_at не возвращают состояние назад. Ошибка откатывает всю страницу; очередь и исходные снимки остаются для повторной попытки.

Binding определяется по Collector connection, device ID и scope. Hostname/peer name не используется для привязки. Real и simulation разделены; старая simulation-сессия не становится текущей. Один binding/source/scope/port обновляет один активный инцидент. EventCount считает изменения объявления, а не опросы. После review следующее изменение создаёт новую occurrence.

Signal.topology хранит binding ID, before/after snapshot IDs, хеши local-port/peer identities, protocol, count и expiry. Raw Neighbor, management addresses и произвольные descriptions не копируются. Все переходы сохраняют immutable source-record references. Качество времени в correlation — estimated; факт корреляции не означает установленную причину. В граф влияния привязывается только наблюдающее оборудование, без выдуманной идентичности удалённого соседа.

## API и review

- GET /api/incidents?resource_kind=topology
- GET /api/incidents/{id}/topology-records?limit=100&offset=0 — ссылки на исходные снимки.
- POST /api/incidents/{id}/review-topology-event — confirmed=true, актуальный expected_event_count и reason.

DataRead разрешает просмотр; IncidentWrite operator/admin разрешает review. Live роль и сессия проверяются в БД, тело JSON закрыто. Любой необработанный снимок того же agent/device/source блокирует review до обработки: это намеренно консервативная защита. Retry подтверждения не создаёт второй audit. RecoveryEligible остаётся false: оператор завершает разбор сообщения, не подтверждает физическое восстановление. Другие участники связанного инцидента остаются открытыми. Команды Agent не отправляются.

OpenAPI 0.41.0 описывает evidence/record DTO и маршруты. Reports принимают resource_kind=topology. Frontend и Agent не изменены. B12 всё ещё требует общей приёмки, оставшихся pattern rules и решения объёма дополнительных topology правил; B13–B16 впереди.
