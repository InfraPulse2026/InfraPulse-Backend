# Проверки B12.2a

Проверены go fmt, go vet ./... и go test ./... -count=1. В общем прогоне этого этапа DB/process tests пропускались; их не выдаём за повторную живую приёмку Agent.

Отдельный прогон на настоящей изолированной PostgreSQL 18 (127.0.0.1:55432) охватил TestPrometheus, TestExternalMonitoring, TestCorrelation, TestAssetRegistryUpgrade и TestZabbix: все прошли, storage 66.500s. Схемы временные и удаляются тестами.

HTTP fixtures проверяют firing/pending, однозначное соответствие selected target, повторные alerts, скрытие raw labels/annotations, неоднозначные pools, неправильное время и partial warnings. Старые Prometheus tests подтверждают отсутствие дополнительного запроса /alerts при выключенном opt-in.

PostgreSQL tests проверяют устойчивый event/incident ID, source ownership, новый activeAt, сохранение open при пустом ответе, передачу в correlation, запрет чужого target/выключенной policy и откат alert event + incident при SQL-сбое. Upgrade сохраняет старые данные. Прежние Zabbix и B12.1 сценарии проходят.

Собраны Windows/Linux backend и Linux automation worker. Agent repository не изменён. Реального Prometheus endpoint команды нет; это HTTP/TLS fixtures + PostgreSQL, а не live acceptance Prometheus 3.15.0.

Явное resolved delivery (Alertmanager), producers hardware/log/drift/topology и финальная B12 приёмка остаются. [Инструкция](backend-v2-b12-2a-prometheus-alerts.md).
