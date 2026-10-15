# Этап 4: SIP/телефония

Этап закрывает пункт 3 списка: хранение и API для доступности SIP, задержки и
результатов проверок. ZIP содержит также topology, availability и logs/audit.
Изменения остаются локальными; GitHub и исходники Agent не менялись.

## Что теперь делает backend

Agent уже выполняет SIP OPTIONS через UDP/TCP/TLS. Раньше backend только
архивировал TelephonyUpdate целиком. Теперь в той же транзакции создаются
проверки SIP-точек, а REST позволяет читать состояние, историю и сводку.
Backend сам не звонит и не отправляет сетевые проверки на адреса из API.

Путь данных:

```text
SIP-точка → Agent OPTIONS probe → существующий gRPC stream
                                  ↓
                      inbox + SIP samples в PostgreSQL
                                  ↓ COMMIT
                      application ACK → Agent
                                  ↓
                   REST: точки, карточка, история, сводка
```

Миграция 007 добавляет:

| Таблица | Назначение |
|---|---|
| `telephony_batches` | Пакеты Agent и количество проверенных точек |
| `telephony_targets` | Идентичность точки и ссылки на актуальную проверку/ошибку часов |
| `telephony_samples` | Каждая переданная проверка, latency, response code и state |
| `telephony_policies` | Ожидаемый интервал проверки конкретной точки |

Прошлые миграции не переписываются. Новая версия применяет 007 при старте;
прежние устройства, метрики, логи и топология сохраняются. Один пакет
фиксируется полностью. SQL ошибка на второй точке откатывает inbox, batch,
обе точки и samples; подтверждения частичного сохранения нет.

## Почему SIP-точка отдельно от устройства

В текущем agent.proto TelephonyHealth содержит target_id, но не device_id.
Идентичность — `(agent_id,target_id)`. Один target ID у разных collectors
сохраняется отдельно; detail без agent_id при неоднозначности возвращает
409 `ambiguous_target`.

Backend не связывает SIP-точку с роутером по совпадению IP и не создаёт для неё
device. Таймаут SIP не делает устройство offline. Inventory и доступность
сети продолжают оцениваться своим модулем. Точная связь target → device
потребует согласованного явного сопоставления в будущем.

## Какие данные доступны frontend

В `latest` сохраняются ID сообщения и проверки, address, transport, source,
reachable, response_code, latency_ms, consecutive_failures, state,
error_category, observed_at и received_at.

- `observed_at` — время проверки Agent; `received_at` — время приёма backend.
- `latency_ms: null` — задержка отсутствует; `0` — переданный измеренный ноль.
- `response_code: null` — код ответа не получен; он не заменяется кодом 200/0.
- При отсутствии квалифицированной проверки `latest: null`.
- `source=simulated` означает синтетические данные; обычный collector
  передаёт `sip.options`. На экране это нужно различать.

Состояние, вычисленное для текущего экрана, находится в `health`:

| status | Значение |
|---|---|
| `up` | Свежий Agent report успешного состояния |
| `degraded` | Свежий Agent report деградации |
| `down` | Свежий Agent report отказа SIP-точки |
| `stale` | Истёкло наблюдение либо потеряна связь с collector |
| `unknown` | Нет квалифицированных данных, неизвестное/противоречивое состояние или ошибка времени |

Agent сам учитывает latency/failure thresholds; backend не изобретает другой
порог вместо Agent. Совместимый state `healthy` отображается как `up`, сохраняя
переданное значение в sample. `reported_state` и `reported_reachable` сохраняют
последнюю квалифицированную оценку даже при stale. Нет данных — `null`.

У текущего Agent совпадающий SIP ответ 401/407 означает достижимую точку,
но результат OPTIONS — degraded/authentication_required. Это не timeout.
Проверка 401 → 200 → timeout → recovery выполнена с обычным Agent на локальной
UDP fixture. Backend сохраняет его значения без подмены неудачи успешным ответом.

Карточки, список и summary используют одну функцию оценки. В summary
`total = up + degraded + down + stale + unknown`. `clock_skew` и
`agent_disconnected` — пересекающиеся диагностические счётчики, их не прибавляют к total.

## Свежесть и интервалы до 24 часов

В protobuf не передаётся фактический SIP probe interval. Backend поэтому
хранит ожидаемый интервал отдельно и честно показывает, что он не подтверждён
Agent. Default — 30 секунд, как у текущего Agent endpoint defaults.
Переменная `DEFAULT_TELEPHONY_INTERVAL` задаёт общий backend default; она
добавлена в .env.example/Compose. Допускается целое количество секунд 3..86400.

У конкретной точки можно сохранить override через monitoring API. Допуск
до stale: `2 * interval + min(30, max(5, interval / 10))` секунд.
Для 3s это 11s; для 30s — 65s; для 24h — 48h30s. Связь с collector отдельно
оценивается через AGENT_TIMEOUT: даже при суточном SIP interval Agent должен
продолжать heartbeat. Потеря collector не выдаётся за доказанный отказ SIP.

PUT сохраняет только ожидание backend. Ответ содержит:

```json
{
  "expected_interval_seconds": 86400,
  "stale_after_seconds": 172830,
  "origin": "target_override",
  "delivery_mode": "manual_agent_yaml",
  "application_status": "unverified",
  "agent_yaml_fragment": "telephony:\n  endpoints:\n    - id: \"pbx-main\"\n      interval: 86400s\n"
}
```

Фрагмент нужно объединить с существующей записью endpoint в YAML Agent,
сохранив address, port, transport и остальные настройки. Он не является
полным конфигом Agent. Код Agent не меняется. Live ConfigUpdate — отдельный
этап, требующий согласования контракта с его владельцем.

## Поздние сообщения и неверные часы

Повтор envelope с тем же message_id не создаёт новую проверку. Изменённый
business payload под этим ID отклоняется. Разные сообщения могут содержать
новые проверки одной точки с одинаковым observed_at — они остаются в истории;
при равенстве дат выбор уточняется received_at/id.

Поздняя проверка с меньшим observed_at не заменяет более новую. Дата более
чем на 30s позже server received time помечается clock_skew и сохраняется
как диагностическая проверка, а не latest. Карточка остаётся на последнем
корректном sample. Если его нет, состояние unknown/clock_skew.

`clock_issue` хранит последнюю такую ошибку; `health.clock_skew` показывает,
актуальна ли она относительно latest. После более нового квалифицированного
report флаг очищается, история ошибки остаётся. Нормальный sample после
исправления часов не блокируется будущей датой. Регресс часов самого сервера
тоже не делает будущую относительно текущего времени проверку здоровой.
Пустой совместимый update не удаляет существующие точки.

## Маршруты и примеры запросов

| Метод/путь | Ответ |
|---|---|
| GET `/api/telephony/targets` | Список `items/limit/offset` |
| GET `/api/telephony/targets/{id}` | Карточка с latest, clock_issue, health, monitoring |
| GET `/api/telephony/summary` | Текущие счётчики состояний |
| GET `/api/telephony/targets/{id}/history` | `items/next_before_id/limit` |
| GET `/api/telephony/targets/{id}/monitoring` | Ожидаемый интервал и YAML fragment |
| PUT `/api/telephony/targets/{id}/monitoring` | Сохранить ожидаемый интервал |

Список фильтруется по agent_id, target_id, source и transport=udp/tcp/tls;
порядок — agent_id/target_id. Limit 1..500, default 100; offset 0..1000000.
Summary принимает необязательный agent_id, считает все точки этого collector.

History принимает limit 1..500, before_id и from/to по observed_at.
from включительно, to исключительно; RFC3339 с часовым поясом.
Проверки идут по ID вставки убывающе, включая late delivery и ошибки часов.
Для следующей страницы передайте next_before_id как before_id с теми же
фильтрами. Последняя страница содержит null; пустая — items=[].
Для графика учитывайте observed_at и пропуски latency, не рисуйте null как ноль.
Пагинация не фиксирует неизменяемый снимок при параллельном приёме данных.

Примеры при запущенном backend:

```text
http://127.0.0.1:8080/api/telephony/targets
http://127.0.0.1:8080/api/telephony/targets?transport=tls
http://127.0.0.1:8080/api/telephony/summary
http://127.0.0.1:8080/api/telephony/targets/pbx-main/history?limit=100
```

Target ID в URL кодируется через encodeURIComponent, при необходимости
добавляется agent_id. PUT body: `{"expected_interval_seconds":30}`.
Другие поля, дробные значения, 2s/более 24h и лишний JSON отклоняются с 400.
Неизвестная точка — 404; policy не создаёт фиктивную точку.
Frontend контракт находится в OpenAPI 0.6.0.

## Проверки и границы

Восемь новых PostgreSQL tests проверяют поля/null/0, chronology/replay/conflict,
256 точек в одном пакете, Agent isolation и ambiguity, time/cursor history,
пустой update, clock reset, 3s/24h policies, expiry/disconnect и SQL rollback.
Unit/API tests проверяют healthy alias, invalid transport/code/latency, поля
фильтров, manual/unverified, границы JSON и ответы ошибок.

Тест `TestUnmodifiedAgentSIP401TimeoutAndRecovery` запускает отдельно собранный
обычный Agent с durable queue против локальной UDP fixture. Controller и
PostgreSQL настоящие, schema отдельная. Проверены 401 → up по 200 → down после
таймаутов → up и сброс failure counter; историю можно прочитать из Store.
Для воспроизведения задаются AGENT_BINARY и TEST_DATABASE_URL, затем:

```sh
go test ./internal/integration -run TestUnmodifiedAgentSIP -count=1 -v
```

В тесте интервал Agent 500ms выбран для короткого запуска; он не изменяет
production configuration. Без этих env variables process test пропускается.

Отдельный запуск полного backend и неизменённого Simulator: sip-recovery,
7s, interval=1s, четыре точки. Получены 7 batches/28 samples; каждая точка
прошла up → degraded → down → up. После остановки collector сводка содержит
четыре stale, а latest сохраняет up. REST примеры, history страницы,
timeline и manual policy на 24h находятся в docs/examples/telephony.*.simulated.json.

Это SIP OPTIONS health, не качество RTP/аудио, не состояние реальных звонков
и не проверка регистрации абонента. UDP fixture не является настоящей АТС;
TCP/TLS collectors в этом этапе не запускались против реальной телефонии.
Сам Agent уже имеет свои probe tests, их запуск относится к его владельцу.
Реальный Linux deployment, race detector и Docker runtime здесь не заявляются
проверенными. Локальный обычный Agent запуск выполнен на Windows.

Старые архивные TelephonyUpdate не backfill-ятся автоматически: новая история
начинается с новых пакетов. Сырые архивы сохранены. Retention, incident
correlation, device binding, auth/roles, commands/rollback и PKI остаются
следующими этапами. API пока development на loopback, без user auth.
В списке со скриншота закрыты пункты 1, 2, 3 и 8 — четыре из десяти.
