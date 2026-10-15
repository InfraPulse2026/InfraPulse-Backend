# Проверки B12.3c — 14 октября 2026

Проверки используют неизменённые protobuf fixtures и отдельную PostgreSQL 18 на 127.0.0.1:55432. Реальная Linux auditd/Agent приёмка на стенде команды в этот этап не входит.

Проверяются явное success=false без severity; отсутствие сигнала при true/unknown; сохранение nullable severity и исходного severity=6; warning без утверждения intrusion; приоритет severe rule; раздельные group keys; старый timestamp; replay без увеличения счётчика; отсутствие автоматического recovery; pending review conflict и последующее подтверждённое закрытие без RecoveryEligible.

Регрессионные проверки покрывают прежние severe-log incidents, permissions/closed JSON/review, OpenAPI, logbook, AssetLogs и populated AssetRegistryUpgrade. Общий go test ./... без opt-in базы не заменяет этот PostgreSQL прогон. Backend собирается для Windows amd64 и Linux amd64. Итоговые результаты и проверки распакованного cumulative ZIP фиксируются ниже после завершения.

Commit/push не выполняются. Пакет исключает runtime, инструменты, тестовую БД, .env, private keys и Git metadata. Предыдущие ZIP сохраняются.

Итог: go fmt ./..., go vet ./..., общий go test ./... и Windows/Linux builds успешны. Focused PostgreSQL AuditFailure/LogIncident/LogReview/LogSeverity/OpenAPI/AssetRegistryUpgrade — storage 10.972s. После расширения pending-review и nullable severity=6 повторены AuditFailure/LogIncident/LogReview/LogSeverity/OpenAPI/Logbook/AssetLogs — storage 30.863s, API 0.110s, health 0.030s, успешно. Agent git status пустой. Распакованный ZIP проверяется отдельным прогоном перед передачей пользователю.
