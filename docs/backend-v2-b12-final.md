# Завершение реализации B12

B12 завершён на уровне локальной backend-реализации и fixture/SQL интеграционных проверок. Приёмка на настоящих Zabbix, Prometheus/Alertmanager, Collector и оборудовании команды остаётся частью B16; код и тестовые ответы не выдаются за такую приёмку. Переходим к B13.

## Что входит

Сохранены прежние incident rules, lifecycle, история и ACK/replay. Нормализованы сигналы Collector metrics/SIP, внешние Zabbix problems, Prometheus alerts с explicit Alertmanager recovery, аппаратное состояние, серьёзные syslog записи и явные audit failures, B11 configuration drift и консервативные LLDP/CDP peer-change события.

Детерминированная корреляция учитывает Asset, проверенные service dependencies, свежую физическую adjacency, площадку, временную близость, качество времени, source diversity и recurring category. Configuration-precedes учитывается только при достаточно точном времени; Ansible timestamps сейчас estimated, поэтому точная последовательность причин для них не утверждается. Совпадение времени или площадки без структурного основания не объединяет инциденты. Общий case закрывается только после завершения всех участников; потеря Collector не означает физический отказ каждого устройства.

Хранятся основания группировки, source-record references, timeline, durable pending и повторная обработка. Simulation изолирована по session/source; привязка оборудования требует source ownership, а не совпадения hostname. Это объяснимая корреляция, не доказанная root cause. Graph не объявляет реальную маршрутизацию SIP из физического соседства.

## Текстовые правила

Добавлен закрытый каталог версии 1: log.kernel_oom_kill и log.ssh_password_failure. Форматы сверены с первичным кодом [Linux oom_kill.c](https://github.com/torvalds/linux/blob/master/mm/oom_kill.c) и [OpenSSH auth.c](https://github.com/openssh/openssh-portable/blob/master/auth.c).

Правила принимают ограниченную однострочную message с точным process tag kernel либо sshd/sshd-session, валидным форматом и прежними ограничениями времени/source binding. Пример строки в документации, произвольные слова error, неверный IP/port, truncated и многострочный текст не дают сигнала. Каталог намеренно не обещает распознавание всех Linux/kernel/SSH версий. OOM record даёт critical reported event; SSH failure — warning, не доказательство атаки. Исходный отсутствующий severity остаётся null. Severe и typed audit имеют приоритет: один record не создаёт несколько сигналов.

У новых pattern групп собственный rule в fingerprint. Replay не увеличивает count. Используются прежние log-records и review-log-event; operator review не исполняет команды и не подтверждает восстановление устройства. В incident DTO уходит pattern_id и ссылки на records; usernames, IP, PID и message captures туда не копируются. Текст остаётся в исходном журнале с прежними правами.

Migration 043 ставит в очередь ограниченный набор недавних кандидатов; worker проверяет полный формат. Кандидат не означает alert, а архив не пересчитывается целиком. OpenAPI 0.42.0 описывает pattern_id.

## Проверки и границы

Заключительный PostgreSQL прогон охватывает LogPatterns, AuditFailure, LogIncident/Review, Correlation, ConfigurationIncident, TopologyIncident, HardwareIncident, Prometheus/Alertmanager, AssetRegistryUpgrade и OpenAPI: storage 43.861s, health 0.037s, API 0.086s, успешно. Сценарий Zabbix + SNMP + SIP через reviewed service из первоначального B12 тоже включён. Параллельные general checks/builds и extracted ZIP проверяются перед передачей.

Для живой приёмки команды потребуются источники и device/Linux Worker стенд. Дополнительные vendor patterns, доказанный physical disappearance, ретроспективный forensic replay и автоматическое объединение/разделение уже созданных cases не являются обещанной функциональностью этого MVP. Лимиты case/detail и правила freshness описаны в B12.1–B12.5a. Общий функциональный контур теперь готов для AI export B13; дальше не расширяем B12 бесконечными эвристиками.

Итоговые go fmt/vet/test и Windows/Linux amd64 builds с buildvcs=false прошли.
