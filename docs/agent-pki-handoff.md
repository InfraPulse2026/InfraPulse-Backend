# Что проверить владельцу Agent после этапа8

Agent checkout использован только как reference; его код/protobuf не менялись.
Backend ожидает текущий production/enrollment контракт P3. Новый frontend для
проверки не нужен: Admin REST можно вызвать через HTTP client.

1. Получить у владельца Backend: публичный enrollment CA, HTTPS URL, gRPC
   адрес и server_name. Проверить SAN серверного сертификата для этого имени.
2. Сообщить Backend свой UUID из существующего `state_dir/agent-id`.
   Не создавать новую identity поверх работающего Agent state.
3. Получить одноразовый token, записать его только в окружение процесса Agent.
   Включить уже существующие security.mode=production и security.enrollment.
   Конфигурационный пример — [stage8-pki.md](stage8-pki.md).
4. Запустить Agent. Backend должен показать одну запись Agent, connected=true
   и heartbeat; token в metadata использован. CSR и сертификат имеют URI
   `spiffe://infrapulse/agent/<тот же UUID>`.
5. Перезапустить Agent без bootstrap token. Его persisted bundle должен позволить
   подключение с прежней identity. Не удалять bundle/ключ для обычного restart.
6. Проверить renewal: metadata нового сертификата имеет renewed_from; новый
   fingerprint используется при gRPC подключении. Внутри одного процесса
   session_id после reconnect может остаться прежним. Для автоматического
   теста backend использует short-lived fixture; на deploy defaultTTL30суток.
7. На отдельном тестовом Agent отозвать текущий fingerprint через admin REST.
   Готовый stream должен закрыться, reconnect с этим сертификатом отклоняться.
   Для восстановления требуется иной активный сертификат либо новый enrollment
   token и явное восстановление Agent bundle по правилам самого Agent.

Token/CA private key/Agent private bundle не пересылать в обычные logs/screenshots.
Владелец Agent получает только публичный CA и одноразовый token приватно;
`ca.key` и `server.key` ему не требуются. Внешние DNS/firewall/HTTPS proxy
нужно проверить в вашей целевой среде отдельно от loopback process fixture.

Этап8 не добавляет ConfigUpdate. Для изменения интервалов без restart нужно
согласовать новый controller message с владельцем Agent; текущий protobuf
в backend оставлен совместимым с текущим Agent.
