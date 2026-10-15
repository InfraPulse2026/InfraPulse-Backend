# Проверка B11.2

Проверено локально на изолированной PostgreSQL 18:

- migration 034, policy current/history и CAS revision;
- свежий single access-VLAN drift → request → admin approval → durable job;
- worker получает только typed operation без произвольных команд;
- успешный результат создаёт encrypted snapshot, новый drift result и `verified`;
- stale evidence, protected interface, запрещённый VLAN, changed policy и changed Asset/target/baseline блокируются;
- policy, изменённая уже после approval, останавливает job до исполнения;
- request и approval идемпотентны; одна активная remediation на Asset;
- role/route/body/query ограничения и OpenAPI 0.33.0;
- Python adapter принимает только точную action shape, Cisco/Arista module и отклоняет VyOS/лишние поля;
- `go fmt`, `go vet`, Go tests и Python adapter tests.

Открытая приёмка: настоящий Linux Ansible Runner и выбранное SSH-устройство/эмулятор пока не предоставлены. Поэтому `verified` доказан интеграционным тестом полного backend workflow с контролируемым worker result, но физическое изменение VLAN на реальном железе ещё необходимо проверить по инструкции B11.2. Agent/Collector не изменялись.
