# B3.2 — предложения сопоставления и контролируемые merge/split

Автоматический resolver и backfill добавлены следующим этапом:
[B3.3](backend-v2-b3-resolution.md). Ниже сохранено описание B3.2.

Дата: 11 октября 2026. Agent/Collector не менялся. Изменения локальные,
без GitHub публикации. Это продолжение B3.1, не завершённая интеграция Zabbix.

## Что работает

- `GET /api/assets/{id}/candidates?limit=20` возвращает предложения сопоставления:
  matched evidence, противоречия, revision другой карточки, явные separation decisions.
- `POST /api/assets/merge` объединяет два совместимых живых объекта.
- `POST /api/assets/split` переносит выбранную непустую часть bindings в новую карточку.
- `GET /api/assets/{id}/decisions?limit=50&offset=0` возвращает историю решений.
- Карточка Asset теперь имеет `revision` и `merged_into_id`.
- Текущий список `/api/assets` исключает merged/retired записи. Старый ID
  продолжает читаться напрямую как историческая карточка со ссылкой на target.

Все read routes доступны viewer/operator/admin. Merge/split доступны только
администратору. Сессия и роль перепроверяются внутри транзакции, с блокированием,
общим с изменениями доступа. Отозванная сессия не может записать решение.

## Правила сопоставления

Предложения используют root hardware UUID либо root serial + manufacturer
с typed `high` confidence. Значения нормализуются; placeholders/нулевые UUID,
redacted serial и serial из одних нулей не являются usable идентификаторами.
Идентификатор компонента с subject ID или другого resource URI не становится
идентификатором whole system. Legacy поле без typed confidence остаётся
наблюдаемым фактом и может обнаружить противоречие, но не создаёт high match.

IP, hostname, модель не используются как самостоятельное основание совпадения.
Root vendor берётся из факта Device.vendor, а не VendorHint. На этом этапе
vendor сравнивается консервативно без словаря синонимов.

Разный root UUID, serial, manufacturer, несовместимый kind или несколько
противоречивых root identities внутри snapshot дают conflict. Совпавший UUID
не скрывает различие serial. BMC, chassis, system и host не объединяются
между разными kinds. Unknown kind не разрешает manual merge: сначала нужны
достоверные сведения о типе. Simulated scopes не объединяются с real или
другими source/session scopes.

`strong_match=true` — предложение для проверки, **не выполненное объединение**
и не криптографическое доказательство физической уникальности UUID. Кандидатов
может быть несколько; лимит страницы до 50. Автоматический merge не включён.

## Merge по шагам

1. Прочитать обе карточки и список source bindings.
2. Проверить evidence/конфликты и при необходимости физические подписи/документацию.
3. Выбрать binding, который будет источником canonical name/vendor/model/versions.
4. Передать обе revision, canonical binding ID, reason и `confirmed=true`.
5. Backend снова проверяет актуальность и все current snapshots в транзакции.
6. Source bindings переносятся к target, а original external IDs, agent/device IDs,
   facts и identities не удаляются. Source Asset становится merged alias.
7. Изменение, решение и access audit фиксируются вместе; ошибка откатывает всё.

Пример тела:

```json
{
  "target_id": "asset_target",
  "target_revision": 4,
  "source_id": "asset_source",
  "source_revision": 2,
  "canonical_binding_id": "binding_selected",
  "reason": "Проверены серийные номера и подпись на стойке",
  "confirmed": true
}
```

POST требует `Content-Type: application/json`, одну `Idempotency-Key` длиной
1–128 printable ASCII символов и действующую admin session. При cookie auth
нужны прежние CSRF/Origin проверки. Повтор с тем же ключом и прежним запросом
возвращает то же решение и `Idempotent-Replay: true`. Другой запрос под тем же
ключом или устаревшая revision получает 409. Ключ scoped по administrator ID.

Human reason обязателен, до 512 UTF-8 bytes, без control characters. Известные
credential patterns проходят через прежний logbook sanitizer перед сохранением.
Это не гарантирует распознавание произвольного секрета, поэтому reason следует
содержать обоснование решения, а не учетные данные.

## Сохранение canonical выбора и конфликтов

После merge новый inventory чужого binding не переключает canonical source.
У выбранного источника поля обновляются как прежде. Любой принятый свежий
inventory меняет revision, поэтому решение, подготовленное до обновления,
должно быть перепроверено.

Backend сравнивает current root facts всей группы. Если позднее источники
начинают противоречить друг другу, `resolution_status` становится `conflict`.
Ни один источник автоматически не удаляется и не отделяется. Исправление
данных/ручной split возвращает согласованную карточку в resolved/unresolved.
`resolved` означает согласованное ручное сопоставление, не физическую доступность
оборудования или независимую проверку всех источников.

Группа ограничена 128 bindings; обновление inventory и manual операции
сериализованы общей PostgreSQL advisory lock. Это простой защищённый вариант
для текущего монолита, не обещание высоконагруженного/HA resolver.

## Split

```json
{
  "asset_id": "asset_existing",
  "revision": 8,
  "binding_ids": ["binding_to_separate"],
  "canonical_binding_id": "binding_to_separate",
  "reason": "Источники относятся к разным экземплярам оборудования",
  "confirmed": true
}
```

Выбранные IDs должны принадлежать карточке; max 64, без дубликатов. Нельзя
перенести все bindings и оставить пустую live карточку. Новый canonical ID
должен принадлежать выделенной части. Несовместимые факты внутри выделяемой
части блокируют операцию: их следует разбирать меньшими согласованными группами.

Split создаёт **новый Asset ID**, переносит bindings вместе со всей историей
и сохраняет оставшуюся карточку. Если прежний canonical выделен, оставшейся
группе выбирается первый оставшийся binding по ID, выбор виден в карточке.
Это выделение группы, не точный возврат старого merged alias к его прежнему ID.
Для актуальной принадлежности original устройства нужно читать его binding,
а не предполагать, что старый historical alias был восстановлен.

В `asset_separations` сохраняется запрет автоматического обратного объединения.
Кандидат показывает `blocked_by_decision=true`. Разделённые группы наследуют
прежние separation constraints. Последующий явный reviewed manual merge
с reason/confirmation может отменить separation именно объединяемой пары;
ограничения относительно других групп сохраняются через aliases.

## Миграция и проверки

021 применяется после 020: добавляет revisions, aliases, таблицы решений и
separations. Existing facts и original IDs не меняются. Общие API/protobuf
baseline tests остаются; OpenAPI теперь 0.22.0.

Проверены: сравнение сильных/конфликтных identities и component subjects;
PostgreSQL merge → replay → canonical update → split; сохранение fact history,
separation flag, stale revisions, revoked administrator, concurrent stale merges,
атомарный rollback при ошибке decision audit, kind/scope и proper-subset guards.
HTTP тесты проверяют admin-only mutations, confirmation/key, read roles,
replay и 409 conflict. Общий набор тестов проверяет прежний Agent/Collector.

## Что осталось в B3

Автоматический deterministic resolver с контролем неоднозначности ещё не
включён. Также нужны общая projection API для будущих внешних источников,
controlled legacy backfill и приемочный кейс Zabbix + два Collector → один
Asset с тремя bindings. Эти пункты не объявлены выполненными в B3.2.

Локально прошли `go fmt ./...`, `go vet ./...`, `go build ./...`, полный
`go test -count=1 ./...` с PostgreSQL и неизменёнными Agent/Collector executables:
integration 112.336s, storage 177.562s. Дополнительные kind/scope/subset проверки
и проверка списка после merge прошли отдельно; после уточнения объяснения
внутренних конфликтов candidate pipeline повторно прошёл manual merge/split test.
Docker/race/настоящий внешний connector требуют отдельной приёмки.
