# Проверки B12.3a — 14 октября 2026

go fmt ./..., go vet ./... и общий go test ./... успешны. Общий прогон выполнен без opt-in TEST_DATABASE_URL/Agent binary: он не заменяет PostgreSQL/device acceptance.

Отдельно на тестовой PostgreSQL 18, 127.0.0.1:55432, выполнены TestHardwareIncident*, TestAssetHardware*, TestCorrelation*, TestAlertmanager*, TestPrometheusAlert*, TestAssetRegistryUpgrade* и TestOpenAPI*: финальный прогон успешен, storage 43.739s, API 1.866s. Каждый DB test создаёт и удаляет собственную временную схему.

Проверены explicit critical/warning, recurrence, отсутствие смешивания Collector sources, запись normalized hardware signal и correlation case, восстановление после трёх distinct OK, сброс после unknown/unsynced, repeated source timestamp, future timestamp без отравления курсора, connection loss без recovery, смена target без использования старого evidence, durable SQL rollback/retry, существующая B8 atomic ingestion и обновление legacy schema с сохранением устройств. На fixtures power Off/zero RPM не создают отдельную проблему.

В промежуточных проверках исправлены тестовые ожидания новой OpenAPI version/path и реконструкция старой схемы в upgrade test: она должна удалять новые dependent hardware queue/state tables внутри только своего disposable schema. После исправлений итоговый прогон, включая upgrade и OpenAPI references, успешен. Отключившийся между рабочими сессиями тестовый PostgreSQL восстановлен и проверки повторены.

Backend собран для Windows amd64 и Linux amd64. Git HEAD backend остаётся a960eb676c3a036397d0c375ccfd0851fb09d9a9: commit/push не выполнялись. Agent reference 215d33a4a5fbc2a12c57d31e516b0ad33f7ae574 остаётся без локальных изменений.

ZIP содержит текущий cumulative backend с B0–B11, B12.1, B12.2a/B12.2b и B12.3a. Старые архивы сохранены. Runtime tools, тестовая БД, Git metadata, .env и private keys в пакет не входят; упаковка проверяет paths и SHA256 каждого извлечённого файла. Реального Redfish/IPMI стенда команды в этой проверке нет: hardware данные поступали через неизменённый protobuf-контракт на тестовых envelopes. Весь B12, B13–B16 и финальная командная приёмка этим этапом не объявляются завершёнными.
