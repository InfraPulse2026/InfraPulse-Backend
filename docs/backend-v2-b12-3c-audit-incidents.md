# B12.3c — явные неуспешные audit-операции

Backend использует существующий AuditRecord.Success из неизменённого protobuf. При success=false создаётся warning по правилу audit.reported_failure, даже без syslog severity. Это сообщение о неуспешной операции, а не доказательство атаки, отказа устройства или причина других симптомов. Success=true и отсутствующее success не создают этот сигнал. Ключевые слова в message и произвольный result не интерпретируются.

Если запись уже имеет severity 0–3, прежнее log.reported_severity имеет приоритет: один record не создаёт два инцидента. Audit-only и severe группы разделены по rule, остальные границы прежние: binding, scope, source, channel, process и пятиминутное окно. EventCount считает различные записи, не уникальные syscall-транзакции. Fragment aggregation по audit event_id в этот этап не входит.

Запись и audit сохраняются до постановки в durable log_signal_pending в одной транзакции. Worker использует прежние ограничения привязки оборудования, источника и сессии. Свежесть проверяется на момент получения; старые timestamps и ненадёжные часы не становятся новыми событиями при перечитывании. Migration 040 ставит в очередь только ограниченный набор недавних неуспешных audit records. Полный исторический пересчёт не выполняется.

API использует прежние /api/incidents, /api/incidents/{id}/log-records и POST /api/incidents/{id}/review-log-event. ResourceKind остаётся log, а rule_id различает audit и syslog. signal.log.audit_success содержит исходный явный результат; signal.log.severity и severity в record references теперь nullable. Ноль остаётся реальным syslog emergency; отсутствие уровня возвращается как null. При исходном severity=6 и success=false warning относится к audit-правилу, а исходное значение 6 сохраняется.

Operator/admin подтверждает review с актуальным expected_event_count и reason. Необработанные записи блокируют закрытие; успешные последующие операции и тишина не означают восстановление. Review завершает только группу событий, без команд Agent и без закрытия остальных сигналов. Raw, UID/AUID, executable, result и message не копируются в incident evidence; они остаются в исходном журнале с действующими правами доступа.

OpenAPI 0.39.0 описывает nullable severity 0–7. Разработчику frontend нужно показывать отсутствие уровня отдельно от нуля. Agent и frontend не изменены. B12 остаётся открыт: текстовые pattern rules, drift/topology producers и командная приёмка ещё впереди.
