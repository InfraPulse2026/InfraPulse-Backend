# Проверки B12.1

Среда: Windows, локальный Go, изолированная PostgreSQL 18 на 127.0.0.1:55432. Тесты создают и удаляют отдельные схемы. Пользовательская база не используется.

Проверены:

- форматирование и `go vet ./...`;
- права viewer/operator/admin, строгие bodies, обязательный Idempotency-Key, pagination и OpenAPI references;
- scored grouping с обязательной структурной связью; временное окно, исключение resolved и смешения simulated Collector;
- общая service dependency у нескольких источников;
- очередь в одной транзакции с исходным incident; SQL-сбой не создаёт частичную проекцию и оставляет задание для retry;
- repeat/rebuild без дополнительной timeline;
- восстановление одного источника оставляет общий incident открытым, полное явное восстановление закрывает;
- версии и idempotency ручного service mapping;
- fixture-сценарий Zabbix problem + SNMP degradation другого Collector + reviewed SIP target формирует один case с тремя источниками и evidence;
- upgrade существующей базы, прежние Zabbix/health/VoIP/context сценарии;
- Windows и Linux сборки backend, Linux automation worker.

Fixture scenario проверяет backend в PostgreSQL, а не наличие живого командного Zabbix, SIP АТС или SSH-устройства. Новые signal producers Prometheus/hardware/logs/drift/topology changes пока не входят в B12.1. Все ограничения — [описание этапа](backend-v2-b12-1-correlation.md).

Полный `go test ./... -count=1 -timeout=12m` выполнен с TEST_DATABASE_URL, неизменённым P3 AGENT_BINARY и финальным COLLECTOR_V2_BINARY 215d33a: все пакеты прошли. Integration — 151.670s, storage — 433.943s. После завершающих изменений карточки/rebuild выполнен отдельный повторный прогон correlation/API/storage tests — успешный.
