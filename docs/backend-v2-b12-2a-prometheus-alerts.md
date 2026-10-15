# B12.2a Prometheus firing alerts

Backend теперь принимает действующие Prometheus alerts и включает их в прежний source-owned Incident Engine и общий B12.1 correlation layer. Agent и frontend не менялись.

## Включение

В существующем admin DataSource типа PROMETHEUS установите `configuration.prometheus.alerts_enabled=true`. По умолчанию параметр false, поэтому старые установки продолжают прежний bounded inventory/metrics sync. PUT DataSource требует текущую revision и полный существующий input; сохраняйте прежние scrape pools, identity labels, mappings и TLS/auth settings.

Пример только изменяемой части конфигурации:

```json
{
  "prometheus": {
    "scrape_pools": ["node"],
    "identity_labels": ["job", "instance"],
    "page_size": 5,
    "max_targets": 20,
    "alerts_enabled": true,
    "auth_mode": "none",
    "device_type": "server",
    "profile": "node_exporter"
  }
}
```

Это фрагмент configuration, а не полное тело PUT. Для bearer/basic используйте прежнее encrypted credential поле DataSource; пароли и токены не входят в labels или URL.

## Путь данных

Существующий источник выполняет read-only GET `/api/v1/alerts` на разрешённом origin через тот же защищённый transport. Общий список сверяется со всеми выбранными targets, а публикация ограничена текущей страницей target sync. Alert должен соответствовать ровно одному selected target по всем identity_labels. При совпадении в нескольких scrape pools или отсутствии target запись не привязывается по догадке.

Только state=firing становится incident; pending пока не является подтверждённым problem. Warning — severity по умолчанию; critical/fatal/disaster отображаются как critical. Имя проходит повторную фильтрацию секретов. Произвольные annotations и исходные labels не сохраняются и не экспортируются.

Identity: source instance + target ID + SHA-256 полного набора source labels и activeAt. В labels учитывается полная source identity, но в БД сохраняется только fingerprint. Внутренний numeric event ID создаётся backend; это не native ID Prometheus. Повторные опросы дают один внешний event и один legacy incident. Новый activeAt означает отдельное событие. API `/api/signals` и source history сохраняют происхождение PROMETHEUS; legacy rule ID — `prometheus.alert`. Затем durable correlation worker связывает incident с глобальным Asset и reviewed service/topology context.

Inventory, alert event, incident, correlation_pending и sync result сохраняются внутри прежней SQL-транзакции задания. Ошибка не даёт частичного успешного sync.

## Время и recovery

`activeAt` сохраняется как source timestamp возникновения; повторный опрос обновляет last_seen, а не переименовывает начало события во время опроса. Timestamp вне допустимого диапазона или больше server time +30s отклоняется.

Prometheus `/alerts` возвращает активные alerts, а не журнал явных recovery. Поэтому отсутствие записи, pending, ошибка HTTP, предупреждение partial response, смена scope/config или исчезновение target НЕ закрывают incident. После ожидаемых циклов опроса evidence становится stale/source_unavailable/configuration_changed. Общий B12 incident сохраняет активный member и не выдаёт отсутствие наблюдений за восстановление.

Следующий шаг для полного lifecycle — отдельное аутентифицированное explicit resolved delivery (например, Alertmanager), с проверкой source identity и времени. Оно пока не реализовано. Эта версия реализует приём firing и дедупликацию, а не полную Alertmanager integration.

## Ограничения

Максимум 1000 records в ответе alerts API, 64 labels на запись, 1024 bytes на label value, 100 matched alerts на страницу. Превышение границ или partial warnings делает sync неуспешным; recovery при этом не выдумывается. Учитывается худшее количество страниц selected targets для freshness; для малого стенда выставляйте реалистичный max_targets. Alerts без job/instance/остальных выбранных identity labels не сопоставляются с Asset.

Новая additive migration — 036. OpenAPI — 0.35.0. Предыдущие таблицы и API сохранены. Реальный командный Prometheus endpoint не предоставлен: выполненные tests используют HTTP/TLS fixtures и настоящую локальную PostgreSQL.

Официальная основа контракта: https://prometheus.io/docs/prometheus/latest/querying/api/#alerts

Добавлен [B12.2b — explicit resolved delivery Alertmanager](backend-v2-b12-2b-alertmanager.md). Историческое ограничение отсутствия resolved выше относится к первоначальному B12.2a.
