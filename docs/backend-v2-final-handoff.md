# Передача текущего Backend v2

Использовать один последний cumulative ZIP с каталогом InfraPulse-Backend. Все предыдущие этапы уже внутри; старые архивы не нужно накладывать друг на друга. Сохранять структуру каталогов, в том числе api/internal/cmd/scripts/.github. ZIP не включает Agent, frontend, реальные .env/ключи, базу, инструменты и бинарники.

Основной запуск — README и docker-compose.yml. Production transport, PKI и Linux automation включаются согласно отдельным инструкциям; demo режим не заменяет их настройку. Не удалять существующий PostgreSQL volume при обновлении. Сохранить резервную копию базы и прежние vault keys; применение новых migrations транзакционное, неизвестная более новая версия базы блокирует старый backend.

Для разработчика frontend контракт — internal/api/openapi.json 0.44.0 (GET /openapi.json на запущенном backend) и docs/api.md. API требует authentication; cookie mutations требуют CSRF, изменения используют documented confirmation/revision/idempotency fields. Не хранить ключи источников или модели в frontend. AI сначала показывает preview, затем отправляется отдельное подтверждённое job; создание preview не вызывает модель.

Основные экраны обеспечиваются backend API: Assets и source bindings, inventory/physical locations, global topology, freshness/health/logs/SIP, configuration drift/approval/jobs, correlated incidents с evidence/timeline, AI history и unified reports. Native Collector device_id нужно использовать вместе с agent_id; external target/mapping — с source_instance_id. Asset ID объединяет оборудование, но не является автоматически именем Prometheus series.

Чтение статусов должно учитывать freshness и качество времени. Потеря Collector/источника не доказывает физическую поломку; reported log/topology/config event не превращается в восстановленное оборудование от нажатия review. Duration в reports не является uptime/downtime. Simulation namespace и scope нельзя смешивать с реальными измерениями.

Текущие результаты и открытые испытания собраны в backend-v2-b16-security-recovery.md. Реальный стенд команды, LLM и Linux Ansible acceptance ещё требуются. B15 выполнен как MetricStore + небольшой измеренный baseline; переход на TSDB/Grafana не является требованием для запуска MVP. Полноценный HA/distributed dispatch не заявляется.

Изменён только backend. Agent/frontend и GitHub не изменены. Для согласования с ответственным за Agent передать reference commit 215d33a4a5fbc2a12c57d31e516b0ad33f7ae574 и инструкцию повторной protobuf/process проверки при его обновлении; не предлагать незаметно заменять агентский код.
