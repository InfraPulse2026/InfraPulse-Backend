# Этап9F — контролируемый AI preview и analysis

Backend реализует полный workflow: local preview → явное подтверждение
operator/admin → asynchronous provider request → проверка structured result
→ сохранённая история. Agent/protobuf не менялись, frontend не создавался,
GitHub не обновлялся. Внешний AI по умолчанию выключен.

## Что уходит в модель

Preview содержит destination, model, prompt_version, request_hash, expires_at,
excluded_fields, понятный payload и exact provider request JSON. API key не
входит ни в request body, ни в preview; он добавляется сервером только в
Authorization header при отправке.

Экспорт строится по allowlist, а не путём удаления случайных secrets из полного
inventory/logs. Уходят current numerical fact summaries с rule/status/severity,
sample count/window, условными slope/R²/ETA, simulation flag и относительным
временем. Incident/resource IDs заменяются HMAC aliases с новым случайным
ключом каждого preview; внутри одного payload совпадения остаются согласованными.

Не уходят real IDs, IP/hostname, SIP endpoint/address, source labels, raw topology
evidence, inventory, credentials, абсолютные timestamps, audit/log free text.
Связи diagnosis передаются как enums/aliases; они не объявляются root cause.
Размер payload не больше32KiB, до8related incidents плюс seed.

Для логов передаются counts по syslog severity и локально распознанным tags:
out_of_memory/disk_full/io_error/authentication_failure/link_down/sip_timeout.
Это фиксированные SQL text patterns, не полноценная классификация всех логов.
Исходные строки остаются в БД. Counts относятся к agent/device и окну±5минут
около последнего seed observation, с real/simulation scope isolation.
Diagnosis и log summaries читаются последовательно при создании preview;
они не выдаются за forensic snapshot всех источников в один наносекундный момент.
Payload после создания заморожен и виден до отправки.

Псевдонимизация не делает данные абсолютно анонимными: counts, numeric patterns,
rules и относительные времена всё равно раскрывают характер проблемы.
Администратор должен оценить exact preview для вашей сети.

## API, роли и подтверждение

| Метод/маршрут | Назначение |
|---|---|
| POST /api/incidents/{id}/analysis/preview | body {}, viewer/operator/admin; local preview, без network send |
| POST /api/incidents/{id}/analysis | operator/admin, body preview_id/request_hash/confirmed=true |
| GET /api/incidents/{id}/analyses | история jobs, limit1..100 |
| GET /api/analyses/{analysis_id} | состояние и сохранённый result |

Preview действует10минут, создаётся автором не чаще10раз в минуту. Внешний job
требует analysis.write, того же автора preview и exact hash/model/config.
Cookie mutations требуют CSRF. Idempotency-Key обязателен для job creation.
Один автор имеет не больше одного queued/running job, один preview можно
использовать только для одного job. Повтор того же key/body возвращает его ID;
конфликт, consumed/expired preview или active job дают409.

Получение preview само по себе не отправляет данные. Внешний запуск при
AI_ENABLED=false возвращает503 ai_disabled. Development auth bypass не даёт
создавать неавторизованные previews/jobs через storage.

Пример confirmation body:

```json
{"preview_id":"prv_...","request_hash":"64-hex-characters-from-preview","confirmed":true}
```

Ответ202 возвращает job ID; запрос не держит HTTP соединение до окончания модели.
Polling GET показывает queued/running/succeeded/failed/uncertain.
OpenAPI0.16.0 описывает эти routes, AIResult/Preview/Job/ExportPayload DTO.

## Provider и результат

Провайдер — OpenAI Responses API с фиксированным HTTPS destination
https://api.openai.com/v1/responses. URL нельзя передать в body. Используются
TLS verification, запрет redirect/proxy-by-environment, bounded timeout/response,
store=false, max_output_tokens2048 и strict JSON schema через text.format.
Tools/commands, conversation history и background mode provider не используются.
Prompt требует отделять observations от hypotheses, русское объяснение,
references на supplied facts и ограничения данных.

Result содержит summary, observations, hypotheses, recommendations, limitations.
References проверяются на факты именно этого payload; confidence только low/medium,
recommendation safety — read_only/requires_operator_review. Output text проходит
повторную secret-pattern sanitization. Refusal, incomplete, unknown fields,
неверный JSON/references и превышение response bounds не считаются успехом.

Schema/reference validation не доказывает истинность текста модели. Result —
предложение для human review, не подтверждённая физическая причина. Текст никогда
не преобразуется в shell или Agent command. Существующий guarded commands API
и rollback остаются отдельными, с прежними permissions/confirmation/preflight.

Официальные основания интеграции:
[Structured Outputs](https://developers.openai.com/api/docs/guides/structured-outputs?api-mode=responses)
и [Responses migration](https://developers.openai.com/api/docs/guides/migrate-to-responses).
store=false управляет application response storage, но не гарантирует Zero Data
Retention/отсутствие abuse-monitoring logs или всех видов caching. Условия зависят
от account/project/model: [Data controls](https://developers.openai.com/api/docs/guides/your-data).
Не показывайте на защите лозунг «ничего нигде не хранится» на основании одного
параметра store=false.

## Ошибки, повтор и перезапуск

Перед отправкой worker повторно проверяет actor/session, expiry и текущую
model/config. Claim фиксирует running до network operation. Сам provider
пересчитывает hash exact JSON body перед отправкой. Нет automatic retries.

Transport timeout/error после начала request получает uncertain: модель могла
получить данные/начать обработку. После restart running jobs становятся uncertain,
queued — failed before dispatch. Ни один из них не отправляется снова автоматически.
Для новой попытки нужен новый preview/confirmation с пониманием возможного повторного
расхода/экспорта. Ошибка БД при сохранении результата оставляет running; после
recovery это также uncertain. Raw provider error body/API key не записываются в logs.

Confirmed job и access audit сохраняются атомарно. Validated result и audit тоже
сохраняются вместе. Preview/job metadata/payload/results лежат в PostgreSQL
через migration016. TTL запрещает позднюю отправку, но не удаляет историческую
preview запись: автоматический retention cleanup не реализован.

## Как включить на своей машине

По умолчанию:

```text
AI_ENABLED=false
AI_MODEL=
AI_TIMEOUT=45s
OPENAI_API_KEY=
```

Для реального provider test выберите доступную вашей организации модель с
Structured Outputs/Responses support, задайте AI_MODEL и OPENAI_API_KEY в приватном
окружении backend, AUTH_MODE=required, AI_ENABLED=true. Timeout1..60секунд.
Не пересылайте key в чат/скриншот/GitHub; .env не входит в source ZIP.
Модель по умолчанию не выбирается и цена/доступность этой среды не обещаются.
При Compose ENV поля уже пробрасываются в backend; Docker runtime здесь не проверялся.

После restart: login → preview → прочитать exact payload/model/destination →
confirmation/hash+Idempotency-Key → polling job → проверить результат/refs.
Если model/config менялись, создайте новый preview. API previews доступны и при
выключенном provider, чтобы можно было обсудить безопасность без расходов.

## Фактически выполненные проверки

Unit tests: allowlisted export с sentinel IP/ID/secret strings, hash integrity,
JSON schema/reference/enum checks. HTTP fixture проверяет exact reviewed body,
Authorization header отдельно, no duplicate call, redirect/refusal/incomplete
rejection. Это локальный фиктивный ответ, не ответ реальной OpenAI модели.

PostgreSQL tests: frozen preview/job, idempotency, validated result, single-use
preview, disabled/unconfirmed/expired requests, actor logout до claim, restart
uncertain/no resend, audit SQL failure без partial job. Worker fixture проходит
queued→running→succeeded и сохраняет ровно одну обработку с local mock Provider.
API/config tests покрывают viewer preview/operator send, cookie CSRF, explicit
model/key/auth и invalid settings. Общий прогон сохраняет предыдущие реальные
Agent SIP/commands/rollback/PKI tests; Agent source не изменялся.

Реальная OpenAI модель/ключ/стоимость и качество её диагностики пока не проверены.
Также не подтверждены целевой Docker/native Linux/hardware deployment и внешний
HTTPS proxy. Это остаётся обязательной частью финального совместного acceptance.

Основные incidents/reports/context/AI функции пункта10 теперь реализованы в MVP,
но его live-provider acceptance пока открыто. ConfigUpdate пункта6 требует
согласования контракта с владельцем Agent. Не объявляем весь проект готовым к
защите до этой проверки и проверки вашей целевой среды.
