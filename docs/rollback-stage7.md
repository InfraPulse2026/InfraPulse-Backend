# Этап 7: snapshot references и RollbackAction

Этап закрывает пункт 5: backend понимает полученный snapshot_id, связывает его
с исходной командой/Agent/device/interface и отправляет существующий typed
RollbackAction. Код Agent и protobuf не меняются; frontend остаётся другой
команде. ZIP включает предыдущие этапы, GitHub не обновлён.

## Что хранится, а что неизвестно

Содержимое снимка находится в durable command store Agent. Backend получает
только snapshot_id и результат/verification команды. Таблица command_snapshots
хранит наблюдённую ссылку, её происхождение и жизненный цикл, а не копию
конфигурации оборудования.

- expires_at=null, expiry_basis=not_transmitted_by_agent: точный TTL неизвестен.
  Это не означает бессрочный снимок. Agent проверяет срок при исполнении.
- initial_observed_admin_up — исходное измерение backend перед командой.
  previous_state_basis явно говорит backend_initial_observation_not_agent_snapshot.
  Это не доказательство значения внутри фактического снимка Agent.
- after_admin_up — подтверждённое итоговое состояние исходной команды.
- При rollback фактическое восстановленное состояние берётся из verification
  Agent. Backend не заменяет RollbackAction обратной командой enable/disable.

Миграция 010 добавляет registry и ссылки rollback_of_command_id/
rollback_snapshot_id. Существующие ссылки из stage6 results переносятся,
пользователи, команды и история не удаляются. Старые известные ссылки
помечаются superseded при появлении более новой ссылки того же ресурса.

## Запрос оператора

POST /api/commands/{original_command_id}/rollback, role operator/admin,
Content-Type application/json, Idempotency-Key:

```json
{
  "expected_admin_up": false,
  "timeout_seconds": 30,
  "confirmed": true
}
```

expected_admin_up — текущее наблюдённое состояние перед откатом. Agent/device,
канонический интерфейс и snapshot ID выбираются из исходной подтверждённой
команды. Клиент не может подставить другой target или произвольный snapshot.
Cookie требует CSRF. Viewer может читать ссылки/результаты, но не создавать rollback.

Для стенда должны быть явно включены COMMANDS_ENABLED,
COMMAND_ALLOW_ROLLBACK и COMMAND_ALLOW_DISABLE. Последний нужен даже при
ожидаемом восстановлении up: фактический baseline Agent не передаёт заранее,
поэтому rollback рассматривается как право менять состояние в обе стороны.
По умолчанию все mutation flags выключены. Agent отдельно должен разрешать
commands и enable_mutations; его policy и защиты продолжают действовать.

Backend требует успешную verified исходную команду, candidate reference,
совпадающее текущее after-state, отсутствие другой активной/неопределённой
команды Agent, supported resource.rollback и прежние fresh/online/RouterOS/
interface/management-protection checks. Права и reservation проверяются ещё
раз при dispatch. Agent окончательно проверяет ownership, expiry, supersession,
canonical identity/version/aliases и текущее состояние своего снимка.

202 означает queued, не completed. Повтор с тем же actor/key/body возвращает
200 и прежний command ID без повторного действия. Изменение параметров под
тем же ключом даёт 409. По wire передаётся именно Command.Rollback(snapshot_id)
с HUMAN initiator и backend requested_at/timeout.

## Жизненный цикл ссылки

| status | Значение в backend |
|---|---|
| candidate | Подтверждённая ссылка; ещё нужны текущие preconditions и проверки Agent |
| reserved | В одной транзакции создан rollback и зарезервирована ссылка |
| consumed | Agent подтвердил успешный rollback |
| superseded | Более новая наблюдённая Agent snapshot reference того же ресурса |
| uncertain | Доставка/исполнение rollback неизвестны; автоматического повтора нет |
| unverified | Ссылка есть, но подтверждённого success нет |
| unavailable | Agent сообщил отказ/неуспех rollback; ссылка не предлагается повторно |

Создание команды, reservation, event и access audit фиксируются вместе.
Result/inbox/command/event/snapshot transitions также атомарны до MessageACK.
Ошибка БД не оставляет consumed без сохранённого результата.

Если отправка гарантированно не начиналась либо queued отклонён до Send,
reservation освобождается. Это не автоматический повтор: новый запрос требует
нового подтверждённого намерения. При возможной доставке ссылка остаётся
uncertain, а Agent заблокирован для новых мутаций до разрешения результата.
После success старый snapshot consumed, а новый snapshot, созданный самим
Agent перед rollback, сохраняется как отдельная ссылка его новой команды.

Повтор старой ссылки с новым ключом после consumed/superseded получает409.
Ответ Agent snapshot_expired/snapshot_conflict сохраняется как evidence;
backend не выдумывает дату истечения или способ обхода отказа.

## REST для просмотра

| Метод и путь | Результат |
|---|---|
| GET /api/snapshots | items/limit/offset, filters agent_id/device_id |
| GET /api/commands/{id}/snapshot | Ссылка, созданная данной командой |
| POST /api/commands/{id}/rollback | Новая typed rollback команда или replay |
| GET /api/commands/{rollback_id} | Состояние, result и ссылки на origin/snapshot |
| GET /api/commands/{rollback_id}/events | Журнал переходов |

Списки: limit1..500/default100, offset0..1000000. Operation в CommandRecord
теперь бывает rollback; дополнительные nullable поля — rollback_of_command_id
и rollback_snapshot_id. Schema — OpenAPI0.9.0. Snapshot not available даёт
409 snapshot_unavailable; права/policy/protection —403; неизвестная исходная
команда —404. Поле candidate нельзя отображать как гарантию Agent execution.

## Проверки и оставшиеся ограничения

PostgreSQL tests проверяют typed wire ownership, consumption/replay, changed
state/capability/policy, snapshot change перед claim, never-sent release,
uncertainty, supersession, Agent expiry evidence, SQL rollback всей проекции
и миграцию данных stage6. API tests проверяют роли и явно неизвестную expiry.

TestUnmodifiedAgentRouterOSRollbackThroughAuthenticatedREST запускает обычный
неизменённый Agent с временным state/CA против loopback RouterOS HTTPS fixture.
После подтверждённого disable Agent выполнил настоящий RollbackAction,
восстановил up и подтвердил чтением; replay не дал нового PATCH, consumed
reference была отклонена. В полном сценарии четыре PATCH: исходная проверка
disable/enable этапа6, новое disable и один rollback. Пример:
docs/examples/rollback.fixture.json, source local_routeros_https_fixture.

Тест не является проверкой настоящего MikroTik или Linux deployment. Docker
runtime и race detector здесь не заявляются выполненными. Agent-managed
snapshot content/export и его TTL через текущий wire недоступны. Полная
reconciliation неопределённости/ручной unlock остаются дальнейшей задачей;
небезопасного force-resend нет. Подробные результаты — validation.md.

Теперь в объёме MVP реализованы пункты1,2,3,4,5,8,9. Остаются live ConfigUpdate,
production PKI и incidents/reports/AI. Для ConfigUpdate потребуется согласовать
расширение контракта с владельцем Agent: такого controller message сейчас нет.
