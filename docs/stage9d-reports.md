# Этап9D — отчёты и сохранённые JSON snapshots

Backend теперь формирует сводку обнаруженных incidents за период, группирует
правила и ресурсы, показывает повторные occurrences и качество полученных
данных. Agent/protobuf не менялись, frontend не создавался, GitHub не обновлялся.
Это модуль reports; correlation/AI ещё не реализованы.

## Что возвращает отчёт

- Число episodes, пересекающих период, новых и перенесённых из прошлого.
- Resolution/acknowledgment events внутри периода и active episodes на его конце.
- Сумму длительностей episodes внутри периода и средние задержки ack/resolution.
- Группы по rule/source, top ресурсов и повторные occurrences.
- Отдельные labels реальных и simulated источников.
- Число полученных telemetry/SIP samples, missing CPU/latency, clock anomalies
  и сильно поздних telemetry samples. Missing metric может быть unsupported,
  а не failure Collector.
- Версию методики, нормализованные фильтры и время согласованного DB snapshot.

Report availability_percent всегда null. Отсутствие incident не доказывает,
что всё оборудование наблюдалось и работало. Summed incident seconds могут
быть больше длительности периода: две параллельные проблемы считаются отдельно.
Этот показатель нельзя переименовывать в downtime/uptime percentage.

## Период и точная методика

Период полуоткрытый [from,to), от1секунды до31дня. From/to должны быть RFC3339
с timezone, нормализуются в UTC/microseconds. End не может быть в будущем.
Для GET без обоих параметров используется прошлые24часа. Если указан один,
но отсутствует второй, запрос400. Snapshot POST всегда требует оба.

Episode учитывается, если opened_at<to и resolution отсутствует либо позжеfrom.
Длительность = max(0,min(resolved_at либо to,to)-max(opened_at,from)).
Это время обнаруженного Backend incident, не восстановленная полная история
физического нарушения. Начало самой проблемы могло быть раньше устойчивого
срабатывания, а потеря данных не закрывает episode автоматически.

Resolved/acknowledged counts используют event time внутри [from,to).
Resolution ровно вto относится к следующему периоду; в этом отчёте episode
входит в active_at_end и duration ограниченаto. Mean delays считают полное время
от обнаружения до соответствующего event для events внутри периода; без таких
events mean=null, не0. Текущий state строки не подменяет историческое состояние
на границе. Severity breakdown здесь намеренно не вычисляется по изменяемой
последней severity: её нельзя выдавать за точную историческую severity.

Recurrence требует минимум2episodes, открытых внутри этого периода для одного
agent/device/resource kind/ID/context/rule/source/scope. Большой глобальный
occurrence number сам по себе не означает повторение внутри выбранного периода.
SIP contexts и разные simulated sessions не объединяются в «хроническую аварию».

Stats считают все совпавшие incidents в SQL, а не только страницу API.
TopLimit1..100, default20, ограничивает top resources/recurring lists.
ByRule выбирает до1000groups; rule_groups_truncated сообщает возможный лимит.
Quality counts используют received_at и agent/source filters. ResourceKind
не ограничивает raw sample quality, что явно отражено в quality.filter_scope:
например, interface incidents и raw telemetry quality имеют разную гранулярность.

## Как обеспечивается согласованность

Summary выполняется в одном read-only Repeatable Read transaction. Все counts,
groups и quality относятся к одному DB snapshot, даже если Agent в это время
присылает новые данные. Никакие equipment commands в отчёте не выполняются.

Snapshot сначала получает такую согласованную summary, затем отдельная короткая
write transaction повторно проверяет роль/сессию, сериализует Idempotency-Key,
записывает документ и access audit вместе. Generated_at отчёта может быть
раньше created_at snapshot: это обозначает момент чтения, затем сохранения.

Повтор одного ключа у того же автора и тех же normalized filters возвращает
прежний документ/ID. Другие filters с этим ключом дают409. Одновременные
запросы проверены: получается один snapshot и одна audit запись. Поздние
incidents/new imports не переписывают сохранённый report. Update/delete API
для snapshots здесь нет; оператору нужно создать новый снимок с новым ключом.

## REST API

| Метод/маршрут | Назначение |
|---|---|
| GET /api/reports/summary | живой отчёт; default last24h |
| GET /api/reports/summary?from=...&to=...&download=true | JSON attachment |
| POST /api/reports/snapshots | сохранить отчёт, operator/admin |
| GET /api/reports/snapshots | metadata list, без полного payload |
| GET /api/reports/snapshots/{id} | сохранённый документ |
| GET /api/reports/snapshots/{id}?download=true | скачать сохранённый JSON |

Summary filters: from,to,agent_id,source,resource_kind,top_limit.
ResourceKind: device/interface/sip/agent либо пусто. Snapshot list limit1..100
default20 и offset0..100000. Чтения требуют viewer/operator/admin. Создание
требует reports.write (operator/admin); cookie mutation требует CSRF.
Idempotency-Key обязателен, printable ASCII до128characters. Один ключ не
разделяется между авторами. Ответ201 при создании,200+Idempotent-Replay:true
при повторе. Authentication/role/session rechecks сохраняются.

Пример body (подставьте завершённый период):

```json
{
  "from":"2026-10-08T08:00:00Z",
  "to":"2026-10-08T10:00:00Z",
  "agent_id":"",
  "source":"",
  "resource_kind":"",
  "top_limit":20
}
```

OpenAPI0.14.0 содержит ReportFilter/Stats/Quality/RuleGroup/ResourceGroup/
Recurrence/Summary/Snapshot. Methodology возвращается в самом JSON, поэтому
экспортированный документ сохраняет ограничения интерпретации.

## Запуск и проверки

Запуск прежний. Migration015 добавляет индексы периодов/received_at и
report_snapshots. Ни Agent config, ни интервалы collector не меняются.
Scheduler регулярных snapshots, PDF, email и CSV в этот шаг не входят.
Формат JSON выбран для полноценного машинного API и безопасного скачивания;
внешние provider/сервисы не получают данные автоматически.

Domain tests: period bounds, timezone normalization, stable request fingerprint.
PostgreSQL tests: carry-in/clipped durations, from/to boundaries, event means,
recurrence и simulated separation, concurrent idempotency, immutable snapshot
после новых данных, conflict, audit SQL failure без частичного сохранения,
revoked session и metadata-only list. Эти report fixtures используют синтетические
incident timestamps, не выдают длительности за реальный outage железа.

API tests проверяют защищённое чтение, JSON attachment, viewer/operator/admin,
cookie CSRF, period query validation. Общий прогон включает предыдущие реальные
Agent SIP/commands/rollback/PKI fixtures. Native Linux/Docker runtime и реальное
сетевое оборудование не заявляются проверенными в этой среде.

Следующие модули — связанный контекст симптомов и AI preview/analysis с явным
контролем экспорта. ConfigUpdate по-прежнему требует расширения контракта
владельцем Agent. Пункт10 ещё не полностью закрыт; готовых исходных пунктов8из10.
