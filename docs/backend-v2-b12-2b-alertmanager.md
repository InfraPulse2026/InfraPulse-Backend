# B12.2b Подтверждённое восстановление через Alertmanager

Приём Prometheus firing из B12.2a теперь дополнен отдельным resolved webhook. Backend закрывает только однозначно сопоставленный уже сохранённый сигнал. Общий correlated incident закрывается, когда явно восстановились все members. Agent и frontend не изменялись.

## Настройка по шагам

1. В PROMETHEUS DataSource включите alerts_enabled и настройте selected targets. Успешно выполните sync. Migration 037 добавит matching metadata; старые события без metadata должны сначала повторно прийти через polling.
2. Под admin с обычной REST авторизацией выполните PUT `/api/data-sources/{id}/alertmanager`:

```json
{"enabled":true,"expected_revision":0,"ttl_seconds":86400,"confirmed":true,"reason":"Команда проверила источник Alertmanager"}
```

3. Ответ содержит receiver revision, source_revision, expires_at и одноразово выданный token. GET того же endpoint возвращает статус без token. Храните token на сервере Alertmanager в отдельном файле. Backend хранит только SHA-256 token hash. При потере ответа прочитайте GET revision и выполните новую rotation; прежний token станет недействительным.
4. Настройте receiver Alertmanager:

```yaml
receivers:
  - name: infrapulse
    webhook_configs:
      - url: https://backend.example/api/data-sources/SOURCE_ID/alertmanager/webhook
        send_resolved: true
        http_config:
          authorization:
            type: Bearer
            credentials_file: /etc/alertmanager/infrapulse-token
```

Это фрагмент receivers, который команда должна встроить в существующий route. Source ID замените на ID PROMETHEUS DataSource. TLS management API обеспечивается выбранным reverse proxy; integration credential не передаётся в URL. Используется обычный webhook version 4.

5. Alertmanager должен передавать тот же полный набор labels, который Prometheus показывает в /alerts. Добавленные external labels или удалённые labels требуют согласования; backend при несовпадении возвращает 409 и не угадывает recovery. Raw labels/annotations используются в памяти для matching и не сохраняются в SQL/REST.
6. После изменения endpoint, scope или любых source settings нужен новый receiver token, привязанный к новой source revision. Старый token не переносится автоматически на новый источник. Rotation/disable выполняются через PUT с актуальным expected_revision. TTL — 300 секунд..30 дней.

## Защита и идентичность

Webhook использует отдельный source-scoped Bearer; пользовательские admin/viewer/session credentials не принимаются. Auth проверяется до чтения тела, затем повторяется в той же SQL-транзакции, что и recovery. Cookie и несколько Authorization headers отклоняются. Token действует только для конкретного source, его текущей revision, enabled/alerts_enabled и до expires_at. Management PUT/GET требует admin и прежний CSRF при cookie. Rotation сохраняет access audit без секретов.

Проверяются individual status=resolved и startsAt/endsAt. Корневой firing batch может содержать resolved alert; он разбирается по каждому alert. Firing webhook не создаёт новый incident: его источником остаётся выбранный Prometheus polling.

Prometheus activeAt может обозначать начало pending, а Alertmanager startsAt — переход в firing после for. Backend сверяет полный labels fingerprint, source revision, activeAt <= startsAt и startsAt <= first_seen с допуском на clock skew 30s и PostgreSQL precision. Нужно ровно одно совпадение. Неизвестные/неоднозначные события дают 409; весь batch остаётся атомарным. Это консервативное сопоставление, а не обещание восстановить любую историю Alertmanager.

Повторный resolved не создаёт новое evidence/receipt. Поздний firing polling не переоткрывает уже подтверждённо разрешённое событие. Новый activeAt остаётся отдельной occurrence. Сбой записи receipt откатывает изменение source signal/legacy incident/correlation queue целиком.

Тело ограничено 256 KiB, максимум 100 alerts; truncatedAlerts должен быть 0. Неправильное время, future recovery и неизвестные поля отклоняются. Generator/external URLs не запрашиваются backend.

## Проверенные границы

Выполнены PostgreSQL и HTTP tests: разные ActiveAt/StartsAt, правильное закрытие members, replay, unknown identity, rotation, source revision fence и SQL rollback. Реального Alertmanager/Prometheus стенда команды пока нет. Новая migration — 037, OpenAPI — 0.36.0. Hardware/log/drift/topology producers и полная B12 приёмка остаются дальнейшими этапами.

Официальные контракты: https://prometheus.io/docs/alerting/latest/configuration/#webhook_config и https://github.com/prometheus/compliance/blob/main/alert_generator/specification.md
