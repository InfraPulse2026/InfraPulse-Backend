# Проверки B12.5a — 14 октября 2026

Использована отдельная PostgreSQL 18 на 127.0.0.1:55432 и fixtures существующего неизменённого protobuf. Настоящий Agent/LLDP/CDP стенд команды в этом этапе не запускался.

Focused TopologyIncident/TopologyReview/PeerSnapshot/OpenAPI — storage 15.670s, API 7.124s, topology 3.441s, успешно. Расширенный прогон с прежними GlobalTopology/Topology, AssetRegistryUpgrade, configuration/log incidents — storage 57.077s, API 7.149s, topology 3.466s, успешно. После source/target/profile guard повторён focused прогон: storage 15.567s, API 7.149s, topology 3.423s, успешно.

Проверены первая карта без сигнала, refresh/replay без дублирования, typed endpoint replacement, partial без смены baseline, новые изменения/count/occurrence, absence/rediscovery без ложного replacement, stale без recovery, разные Collector bindings, normalized topology category и ровно один наблюдающий Asset. Подтверждённый review не закрывает независимый CPU incident; pending snapshots блокируют review; retry не дублирует access audit; live revoked role запрещена. Принудительный SQL failure оставляет snapshot+pending и откатывает incident, последующая обработка успешна. Unit tests проверяют completeness, ambiguous port, unsupported protocol, clock/future/old timestamps и смену target context. API checks покрывают viewer/operator/admin, закрытые JSON тела и отсутствие raw export.

Общий go test ./... успешен. Первый шаг go build не смог получить VCS status в этой рабочей среде; backend source compile не был причиной. Сборки повторяются с buildvcs=false, без изменения Git-настроек, commits или pushes. Итоговые сборки и проверка extracted ZIP фиксируются после завершения. Распакованный пакет проверяется отдельно перед передачей.

Пакет cumulative, предыдущие ZIP не заменяются. runtime/.env/private keys/test DB/Git metadata исключаются. B12 целиком не объявляется завершённым.

Итог: go fmt ./..., go vet ./..., общий go test ./... и обе Windows/Linux amd64 builds с buildvcs=false успешны. Agent git status пустой. Package paths и SHA256 всех распакованных файлов проверяются упаковщиком; focused PostgreSQL tests и сборка extracted ZIP повторяются перед передачей.
