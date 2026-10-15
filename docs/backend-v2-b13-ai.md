# B13 AI анализ связанных инцидентов

Backend готовит один обезличенный context по связанному инциденту B12. Существующие preview → confirmation → durable job → provider → validated result/history сохранены. Agent и frontend не изменены. Реальный Responses adapter существует и настраивается AI_ENABLED, AI_MODEL, OPENAI_API_KEY, AI_TIMEOUT; модель не выбирается автоматически. Для фактической приёмки реального LLM нужны разрешённый аккаунт/ключ и выбранная модель команды; тестовый HTTP provider не выдаётся за реальный ответ модели.

## Multi source export

Legacy incident preview автоматически использует связанный case, если он уже спроецирован B12. До проекции сохраняется прежний single-source diagnostic fallback. Его локальные text-tag counts остаются поисковыми совпадениями, а не подтверждёнными событиями. Новый case export содержит до 16 сигналов и 32 отношений: категории, закрытый набор rule IDs, numeric facts/counts, относительное время с quality/temporal_usable, состояние, source kind и случайные HMAC aliases sources/assets/resources. Case root names, addresses, raw syslog/audit, configurations, topology payloads, labels и credentials не передаются.

Source type входит в пространство aliases. Одинаковый Asset сохраняет одинаковый alias внутри preview, разные preview имеют новые случайные ключи. Simulation/Collector session isolation сохраняется. Reported events не превращаются в измеренные текущие отказы. Старые/исторические evidence отмечаются stale, их закрытие не доказывает recovery устройства. Configuration-precedes экспортируется только при точном пригодном времени; неизвестное время не становится нулевой задержкой с ложным утверждением порядка.

Fact refs и relation refs проверяются при приёме ответа. Real mapping находится в Preview.references отдельно от Payload/Request: ref → incident/signal/asset IDs либо correlation evidence ID. Эта локальная часть доступна пользователю для проверки, но никогда не добавляется в provider request. Numeric/free-text claims самой модели не доказаны одной только schema validation. Confidence остаётся low/medium; рекомендации read_only либо requires_operator_review, без прав на исполнение.

Case membership/evidence читаются через B12 detail; текущее состояние всех выбранных incidents/freshness — отдельным repeatable-read снимком. Preview фиксируется после чтения. Это ограниченный frozen context с явной revision, не forensic snapshot всех источников в один момент. Current bindings/services в case могут быть изменены оператором; это контекст проверки, не историческое доказательство маршрута трафика.

## API

- POST /api/correlation/incidents/{id}/analysis/preview — тело {}, локальное создание, DataRead.
- POST /api/correlation/incidents/{id}/analysis — прежние preview_id/request_hash/confirmed и Idempotency-Key, AnalysisWrite.
- GET /api/correlation/incidents/{id}/analyses — история, limit 1..100.

Legacy /api/incidents/{id}/analysis и GET /api/analyses/{id} сохранены. Case preview привязан к case ID, автору, выбранному seed и hash; чужой case не принимает его для job. Модель и prompt_version видны в job/history. Новая версия prompt infrapulse-evidence-v2 делает прежние queued previews устаревшими; они не отправляются молча, нужно создать новый preview. Никакой запрос не отправляется при создании preview. Job после confirmation проверяет live роль и сессию; cookie mutation требует CSRF.

## Provider и retry

Сохраняется фиксированный HTTPS destination без user-supplied URL/redirect/proxy, TLS verification, store=false, strict JSON result schema и ограничение request/response. Временной бюджет общий для всего Analyze. Один дополнительный запрос разрешён только после явного rate-limit отказа 429 с известным типом/code; Retry-After соблюдается, без него короткий backoff с jitter. Quota exhaustion и неизвестный 429 не повторяются. Network uncertainty и 5xx не пересылаются автоматически; uncertain сохраняется в job. Никакие errors/body/API keys не копируются в публичное failure поле.

Правила сверены с [официальной документацией OpenAI о rate limits](https://developers.openai.com/api/docs/guides/rate-limits). Это ограниченная политика нашего backend; не обещаем полный retry policy SDK.

## Проверки и статус

Focused PostgreSQL AI/multisource/legacy workflow + exporter/provider/OpenAPI checks прошли: storage 11.728s. Расширенный AI + Correlation прогон storage 21.594s, API 0.096s, analysisai 0.358s, успешно. Проверены реальные локальные references вне request, стабильные aliases внутри одного context, simulation isolation, stale, foreign case rejection, job prompt metadata/history, result refs, consent/CSRF/roles. HTTP fixtures проверяют один bounded retry, отсутствие quota/uncertain retries и сохранение exact reviewed request. Первый unit test выявил отсутствие source type в alias namespace; это исправлено и повторно проверено.

Фактический сетевой вызов с ключом команды не выполнен. Живая LLM/model acceptance остаётся обязательной для демонстрации; локальный функциональный контур готов для B14, она не блокирует разработку отчётов. Итоговые общие builds/tests и extracted ZIP проверяются перед передачей.

Итоговые go fmt/vet/test и Windows/Linux amd64 builds с buildvcs=false успешны.
