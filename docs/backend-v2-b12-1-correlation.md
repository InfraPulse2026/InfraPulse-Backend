# B12.1 Общий инцидент и объяснимая корреляция

Первая часть окончательного B12 реализована поверх существующего движка из 11 правил. Старые incidents, evidence, transitions и API сохраняются. Полный B12 пока не завершён.

## Как проходит сигнал

1. Collector или Zabbix создаёт либо обновляет прежний source-owned incident.
2. В той же SQL-транзакции добавляется `correlation_pending`. ACK по-прежнему выдаётся после commit исходных данных; сложная корреляция не выполняется внутри gRPC приёма.
3. Backend worker каждые 3 секунды берёт до 25 заданий. Проекция и удаление задания фиксируются одной транзакцией. Ошибка оставляет задание для повторной попытки.
4. Один legacy incident получает один `normalized_signals` ID. Внешний event сохраняет прежнюю дедупликацию source + event ID.
5. Backend связывает сигнал с глобальными Assets через существующие SourceBindings. SIP требует включённую reviewed association с совпадающими source, scope, address/transport identity. Hostname/IP не используются для самостоятельного объединения.
6. Детерминированное сравнение выбирает подходящий открытый общий инцидент либо создаёт отдельный. У каждого объединения сохраняются основания и оценки.
7. Карточка возвращает сигналы, timeline, происхождение, confidence и ограничения.

## Когда сигналы объединяются

Окно — 5 минут; минимальная оценка — 60. Общий Asset даёт 50 баллов; reviewed service — 40; свежее соседство с высоким разрешением topology — 40; близость достоверных timestamp — 15; общая площадка — 10; разнообразие источников — 5. Категориальная схожесть на том же Asset даёт 5, но ещё не означает доказанную хроническую проблему. Общая минута и площадка без Asset/service/topology связи не объединяют устройства.

Устаревший сигнал, потеря Collector, изменённая конфигурация внешнего source ограничивают группировку. Они не превращаются в physical offline всех устройств. Неточный timestamp снижает time confidence и исключает утверждение о последовательности configuration change. Для Zabbix используется исходный timestamp события; время повторного опроса не выдаётся за время возникновения.

Real и simulated observations разделены. Одинаковые названия session у разных Collector не объединяют демонстрационные сигналы. Сервисы не могут смешивать Asset namespaces.

Topology evidence содержит edge ID, source, immutable version/observation IDs и время. Confidence — объяснимая оценка группировки; `causal_claim=false`. Физическое соседство не доказывает маршрутизацию SIP или первопричину.

## Восстановление

Общий incident остаётся `open`, пока хотя бы один его сигнал остаётся активным. Восстановление одного источника не закрывает остальные. Отсутствие сообщений и stale не считаются recovery. Когда все member signals явно разрешены прежним движком, общий incident становится `resolved`. Повторная обработка неизменённого incident не добавляет timeline и не повышает revision.

## REST для другого разработчика

Все read endpoints используют `data.read` и доступны viewer/operator/admin после входа:

- `GET /api/correlation/summary`
- `GET /api/correlation/incidents?state=open&asset_id=...&service_id=...&limit=100&offset=0`
- `GET /api/correlation/incidents/{id}` — общий incident, members, evidence, timeline.
- `GET /api/correlation/services`
- `GET /api/correlation/services/{id}`

Admin endpoints используют `access.manage`; при cookie нужен CSRF:

- `POST /api/correlation/services` — reviewed dependency set.
- `PUT /api/correlation/services/{id}` — новый revision.
- `POST /api/correlation/rebuild` с `{ "confirmed": true }` — HTTP 202 и `queued_incidents`, обработка асинхронная. Это повторная проекция, без удаления прошлых группировок.

Service create/update требует один `Idempotency-Key`, `confirmed=true`, reason и expected_revision. Create использует revision 0; update — текущий revision. Повтор возвращает прежний ответ; изменённое тело под тем же ключом возвращает 409.

```json
{
  "name": "Телефония филиала",
  "kind": "voice",
  "enabled": true,
  "expected_revision": 0,
  "confirmed": true,
  "reason": "Команда проверила зависимость сервисов",
  "assets": [
    {"asset_id": "asset_switch", "role": "dependency"},
    {"asset_id": "asset_pbx", "role": "provider"}
  ]
}
```

Сначала настройте Assets/service associations, затем воспроизведите новые сигналы. Service membership сохраняет ручное основание, revision, историю и access audit. Роли provider/dependency/consumer не являются доказательством направления сетевого пути.

## Обновление

При запуске backend применяется новая additive migration 035. Существующие incidents автоматически попадают в durable queue для первой проекции. Данные и таблицы 001–034 не переписываются. OpenAPI — 0.34.0. Новый frontend и изменения Collector не требуются.

## Границы этой поставки

Это B12.1: существующие Collector/SNMP/SIP/CPU/RAM/interface и Zabbix incidents. Prometheus alerts, отдельные producers hardware/logs/audit/configuration drift/topology changes и полный приёмочный сценарий Zabbix + SNMP + SIP — следующая часть B12. Их ещё нельзя считать готовыми.

Группировка ограничена 200 подходящими cases, 256 members на case; карточка ограничивает timeline последними 500 и evidence последними 1000 записями, обозначая `truncated`; topology имеет прежние B7 ограничения. Установка остаётся одиночным Backend. Сохраняется только лучшая доказанная связь при добавлении сигнала; case merge/split и пересмотр старых групп при изменении Asset identity будут отдельной работой. Rebuild обновляет текущие source mappings, но не удаляет историческое evidence группировки. Текущие mappings и сохранённые основания могут относиться к разным revisions — их нужно учитывать при анализе.

Нет внешнего LLM запроса, автоматического ремонта, гарантии причины аварии или production load acceptance. Реальные Zabbix/Prometheus endpoints ещё не предоставлены командой.

После B12.1 добавлен [B12.2a — приём Prometheus firing alerts](backend-v2-b12-2a-prometheus-alerts.md). Историческое ограничение Prometheus выше относится к исходной поставке B12.1; explicit resolved delivery пока остаётся открытым.
