# Этап9C — interface flap и потеря Collector

Agent и protobuf не менялись. Добавлены два backend rule IDs:
interface.flap и collector.unavailable. Теперь health API объявляет11правил.
Frontend не создавался, GitHub не обновлялся. Reports/AI/correlation ещё остаются.

## Частые переключения порта

Backend ведёт persistent tracker и журнал известных переходов up↔down для
agent/device/interface/source/simulated session. Warning: минимум4перехода
за300секунд, critical: минимум8. Одинаковое повторное состояние не увеличивает
число переходов. Один observed_at считается одной точкой; поздние/повторные
наблюдения не переписывают уже принятую последовательность.

Административно выключенный порт, неизвестный admin state/oper state,
изменение boot_id и большой gap прерывают непрерывность. Начальное состояние
после прерывания — baseline, не выдуманный новый физический переход.
Если boot_id отсутствует, continuity дополнительно привязана к Agent session.
Неизвестное состояние не закрывает существующий incident.

Recovery требует up, свежие устойчивые измерения и300секунд без новых переходов
в текущей непрерывности. Quiet interval начинается с baseline либо последнего
известного перехода. Tracker хранится отдельно от окна120telemetry records:
последние120points могут давно забыть начало инцидента, но recovery не теряется.
Тест выполняет303новых измерения с шагом1с после четырёх переключений и подтверждает
закрытие. Это deterministic synthetic fixture, не физическое отключение порта.

Изменения сохраняются в interface_state_changes с telemetry_id и observed_at;
evidence указывает на исходные записи/состояния. В одном snapshot выбирается
до1000переходов окна. При большем числе считайте value нижней ограниченной
оценкой; critical уже подтверждён. Settings версии1 фиксированы и опубликованы
через /api/health/rules. Управления этими thresholds через UI пока нет.

## Потеря Agent — отдельный инцидент

Фоновый worker Backend проверяет100Agents за tick3секунды, с keyset cursor.
Для больших установок полный проход занимает несколько ticks. Worker не
пингует оборудование и не собирает новые метрики: он читает Controller connection
и last_seen. Проверка сериализуется с Hello/heartbeat/disconnect через row lock.

Agent считается доступным, если connected=true и server last_seen укладывается
в существующий AGENT_TIMEOUT. После disconnect/expiry нужно15секунд подтверждённой
потери, прежде чем появится collector.unavailable critical. Это минимум времени,
а не обещание точного события через15секунд при любом размере очереди/БД.
Короткий разрыв до подтверждения не создаёт новый incident.

После старта Backend действует grace=max(15с,AGENT_TIMEOUT), чтобы startup
reset connected не породил ложный шквал аварий. Если Agent не вернулся, loss
подтверждается после grace. Существующий incident не считается resolved от
startup grace или отсутствия данных.

Recovery требует доступной связи не менее15секунд и обновления last_seen хотя бы
через6секунд после начала recovery. Один Hello сам по себе не подтверждает
устойчивую работу. Смена session перезапускает recovery interval. Подтверждение
и восстановление сохраняются в collector_health_trackers, а incident/evidence
пишутся транзакционно. SQL failure не оставляет частично созданный incident.

Resource kind этого incident — agent, resource_id=agent UUID, source=controller,
device_id пустой. CollectorEvidence содержит checked_at, last_seen_at, connected,
agent timeout, loss_since/recovery_since и confirmation seconds. Это факты
Backend о транспорте, а не заявление, что все устройства недоступны физически.
Сообщённое devices.online не меняется на false из-за одного collector incident;
прежняя availability отдельно покажет отсутствие свежих доказательств.

## REST API

- GET /api/agents/{id}/health — состояние Collector rule.
- GET /api/incidents?resource_kind=agent&resource_id=... — его история.
- GET /api/incidents?rule_id=interface.flap — портовые flap incidents.
- Прежние device/interface health routes включают новое flap assessment.
- GET /api/health/rules — все11rules и settings flap/collector confirmation.

Все чтения требуют viewer/operator/admin. Acknowledge operator/admin с CSRF
при cookie остаётся прежним и не исправляет устройство. OpenAPI0.13.0 содержит
agent resource kind, CollectorEvidence и flap fields.

Для collector incident evidence_status=fresh означает свежую проверку Backend,
даже если сам Agent disconnected: это как раз предмет инцидента. Если проверка
не обновлялась60секунд, evidence становится stale. При более чем100Agents
учитывайте время полного прохода и эту freshness границу — worker рассчитан
на один MVP Controller, не распределённый scheduler кластера.

## Запуск и проверки

Запуск прежний. Миграция014 добавляется автоматически, существующие incidents
и evidence сохраняются. Worker запускается вместе с cmd/server и останавливается
с остальными backend workers. CLI admin/backfill не запускают этот worker.
AGENT_TIMEOUT остаётся настройкой backend; YAML/scheduler Agent не меняется.

PostgreSQL tests: четыре переключения/303quiet samples и recovery, сохранение
после Migrate, administrative/unknown без fake flap, короткий disconnect,
confirmed collector loss, отсутствие fake downstream offline, устойчивое
recovery, startup grace и атомарный SQL failure+retry. В этих новых сценариях
используются fixture clocks/SQL connection fixtures; они не выдаются за живое
отключение сети. Общий прогон запускает предыдущие реальные неизменённые Agent
SIP/commands/rollback/PKI process fixtures.

Нет автоматического remediation. Наличие flap и SIP problem рядом пока не
объявляется доказанной причиной. Следующие самостоятельные модули — связь
симптомов, reports и контролируемый AI context/analysis. ConfigUpdate требует
согласования с владельцем Agent. Полностью завершённых исходных пунктов8из10;
пункт10 всё ещё не закрыт целиком.
