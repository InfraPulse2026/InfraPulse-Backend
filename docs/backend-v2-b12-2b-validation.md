# Проверки B12.2b

go fmt и go vet ./... прошли. Общий go test ./... без DB/process opt-in выявил один неверный HTTP status для webhook без credentials; исправлен порядок обязательной auth. Все остальные пакеты общего прогона прошли. После исправления полный internal/api, prommodel и prometheus прогоны успешны.

На отдельной PostgreSQL 18 (127.0.0.1:55432) выполнены Alertmanager, Prometheus, ExternalMonitoring, Correlation и AssetRegistryUpgrade tests: успешны, storage 69.101s. После усиления composite FK migration 037 отдельно повторены Alertmanager/upgrade tests — успешно (9.068s). Это временные удаляемые схемы, не пользовательская БД.

Проверки включают: ActiveAt != StartsAt при for; individual resolved в смешанном batch; отдельный source-scoped Bearer, отказ обычному user token; закрытие только нужного member; общее разрешение после всех recovery; отсутствие дополнительных событий при replay; source revision fence; token rotation; отсутствие token в GET; unknown identity conflict; future/truncated payload rejection; полный rollback при отказе receipt SQL. API scopes и closed bodies проверены.

Windows/Linux backend собраны. Agent repository остаётся чистым; source Agent не изменялся. Реального Alertmanager/Prometheus стенда команды нет. Проверки выполнены на HTTP fixtures и настоящей PostgreSQL, а не объявляются live production acceptance.

Matching намеренно требует единственное уже сохранённое событие с тем же полным labels fingerprint. Extra external labels, неизвестная история, неоднозначность и смена source revision требуют согласования/повторного sync, а не закрытия по догадке. [Настройка](backend-v2-b12-2b-alertmanager.md).
