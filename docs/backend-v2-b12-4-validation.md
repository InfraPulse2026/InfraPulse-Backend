# Проверки B12.4 — 14 октября 2026

Focused ConfigurationIncident/OpenAPI tests прошли на отдельной PostgreSQL 18: storage 10.674s, API 0.085s.

Проверены создание из сохранённого drift, Asset без фиктивного Agent ID, immutable snapshot/result references, отсутствие текста конфигурации/credentials/IP в incident DTO, normalized configuration signal и Asset mapping с estimated time. Исторический compliant не закрывает текущий drift; replay не добавляет evidence; свежий compliant закрывает тот же контекст; следующая проблема создаёт occurrence 2. Тишина, unknown, baseline replacement и disable target не подтверждают recovery. Принудительный SQL failure откатывает incident/state, сохраняет source comparisons и pending для повторной попытки.

Регрессионный PostgreSQL прогон включает B11 drift/remediation, automation, AssetRegistryUpgrade, прежние hardware/log/audit incidents, correlation, OpenAPI и reports. Общие проверки и результаты финального прогона фиксируются после завершения. Настоящий Linux/SSH/device стенд команды здесь не проверялся; существующие protobuf fixtures и транзакции PostgreSQL не заменяют эту приёмку.

Предыдущие cumulative ZIP сохраняются. Commit/push не выполняются; Agent reference остаётся read-only.

Итог: go fmt ./..., go vet ./..., общий go test ./... и builds Windows/Linux amd64 успешны. Расширенный PostgreSQL прогон storage 116.596s, API 0.216s; package reports unit tests включены общим прогоном. После уточнения freshness/normalizer и convention severity=none повторён focused ConfigurationIncident/OpenAPI: storage 10.408s, API 0.094s, успешно. Agent git status пустой. Распакованный cumulative ZIP отдельно проверяется перед передачей.
