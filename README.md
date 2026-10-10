# content-cache для Campbox

npm registry возвращает для некоторых пакетов полный JSON метаданных больше
10 МиБ. Upstream v1.4.0 не сохраняет такие ответы (`payload exceeds maximum
size`), поэтому каждое обращение снова загружает их из интернета. Эта сборка
сохраняет метаданные до 64 МиБ и позволяет менять предел без пересборки.

Основа: [Buildkite content-cache v1.4.0](https://github.com/buildkite/content-cache/tree/v1.4.0),
commit `7c42c286ac0cbb450a6ee0e513894acce15fe1dc`. История upstream сохранена;
remote `upstream` указывает на официальный репозиторий. Модуль Go сохраняет
исходный путь, чтобы правки было проще переносить. Лицензия — Apache-2.0,
см. [LICENSE](LICENSE). Общая документация прокси: [README.upstream.md](README.upstream.md).

## Одновременные запросы npm-метаданных

При промахе кэша одновременные запросы одного пакета используют одну загрузку
из registry и одну запись в кэш. Запросы разных пакетов выполняются параллельно.
Общий результат содержит полные метаданные с исходными upstream URL; формат
ответа и ссылки tarball формируются отдельно для каждого клиента. Поэтому
полный/сокращённый ответы и разные адреса доступа используют одну загрузку.

Отмена одного клиента не отменяет загрузку для остальных. Оставшаяся загрузка
может заполнить кэш даже после ухода всех клиентов; она ограничена timeout и
отменяется при остановке сервера. Ошибки registry передаются текущим ожидающим
клиентам, следующий запрос может повторить загрузку. Ошибка записи в кэш не
мешает отдать успешно полученные метаданные.

Запись выполняется до завершения общей загрузки, чтобы исключить повторный
промах между получением ответа и сохранением. В логах и HTTP-метриках
`cache_result=coalesced` обозначает запросы, присоединившиеся к текущей загрузке.
Объединение работает внутри одного процесса content-cache; несколько его
экземпляров не разделяют выполняющиеся запросы.

## Запись метаданных и очистка

Параллельные записи bbolt объединяются в одну durable-транзакцию с одним
fsync. Первый/одиночный писатель выполняет Update сразу; пересекающиеся записи
используют Batch. Метаданные разных npm-пакетов больше не удерживают общую
блокировку индекса. Read-modify-write состояния tarball сохраняет блокировку.

Настройки задаются без пересборки:

```sh
content-cache serve --storage=/var/lib/content-cache \
  --metadata-batch-size=100 --metadata-batch-delay=1ms
```

Альтернативы: `METADATA_BATCH_SIZE=100`, `METADATA_BATCH_DELAY=1ms`.
Размер и задержка должны быть положительными; размер `1` отключает batching.
Задержка ограничивает сбор группы, а не время ожидания занятой БД или fsync.
Пользовательские callbacks UpdateJSON/UpdateEnvelope остаются в Update:
Batch может повторять callback после отката неудачной группы.

S3-FIFO освобождает блокировки перед удалением файлов и между закреплёнными
кандидатами. При ошибке удаления кандидат возвращается в очередь. Сканирование
ограничено исходной длиной очереди; если все элементы закреплены, очистка ждёт
следующего события или таймера вместо немедленного повторения. Повторное
добавление hash не увеличивает счётчики размера и длины.

Добавлены метрики времени операций/состояния bbolt, ошибок очистки,
отсутствия прогресса и устаревших элементов очереди. Формат БД не изменён.
Замеры до/после, параметры и исходные результаты:
[benchmarks/2026-10-10/README.md](benchmarks/2026-10-10/README.md).

## Лимит метаданных

```sh
content-cache serve --storage=/var/lib/content-cache \
  --listen=172.17.0.1:4874 --metadata-max-size=67108864
```

Альтернатива — `METADATA_MAX_SIZE=67108864`. Аргумент имеет приоритет над
переменной окружения. Единица измерения — байты; допустимый диапазон
1–268435456 (256 МиБ), значение по умолчанию — 67108864 (64 МиБ).
Один предел применяется к записи и чтению всех метаданных. Архивы пакетов
хранятся отдельно; `--cache-max-size` ограничивает общий размер кэша на диске.

Формат metadata.db не изменён. При откате на upstream v1.4.0 записи больше
10 МиБ станут нечитаемыми для старого бинарника. Установщик сохраняет бинарник
и закрытую metadata.db перед обновлением и восстанавливает их при неудачном запуске.

## Разработка и сборка

Go для CI закреплён: **1.26.8**, `golang:1.26.8-bookworm`. На машине разработчика
нужны Go, Git и Python 3. Для race-тестов нужен C-компилятор.

```sh
sh scripts/check.sh
python3 -m unittest discover -s scripts/tests -v
sh scripts/build.sh 1.4.0-campbox.1
```

В `dist/` появятся статические бинарники `content-cache_linux_amd64` и
`content-cache_linux_arm64`, `SHA256SUMS`, `LICENSE` и `BUILD-INFO.txt`.
Последний содержит версию, commit сборки, состояние рабочей копии (clean/dirty),
commit upstream и версию Go.
При запуске сборки без тега используется версия `0.0.0-dev`.

## Подключение к GitLab

Создать пустой проект `tlcamp/content-cache` в `gitlab.campbox.tech`,
включить Package Registry и дать проекту доступ к раннеру с тегом
`docker-executor`. Начальная рабочая ветка — `campbox`; сделать её default branch.

```sh
git remote add origin ssh://git@gitlab.campbox.tech:2222/tlcamp/content-cache.git
git add .
git commit -m 'feat: configure metadata limits and add GitLab releases'
git push -u origin campbox
```

`.gitlab-ci.yml` запускает проверки и сборку на push и MR. Когда у ветки уже
есть MR, отдельный branch pipeline пропускается. CI использует только временный
`CI_JOB_TOKEN`: постоянный токен для публикации не требуется.

## Автоматический релиз

После успешных проверок создать и отправить тег:

```sh
git tag -a v1.4.0-campbox.1 -m 'Campbox metadata limits and GitLab packaging'
git push origin v1.4.0-campbox.1
```

Теги должны иметь вид `v<upstream-version>-campbox.<revision>`. По ним pipeline:

1. Проверяет форматирование, `go vet`, Go-тесты с race detector и установщик.
2. Собирает Linux amd64/arm64 с этой версией.
3. Проверяет SHA256 всех файлов и публикует их в Generic Package Registry.
4. Создаёт GitLab Release со ссылками на опубликованные файлы.

Публикация повторно допускается только для идентичного содержимого. Если файл
с такой версией уже существует и отличается, job завершится ошибкой; для нового
содержимого нужен новый тег. Публикации выполняются последовательно через
`resource_group`. Исходные upstream-теги автоматически не публикуются.
Job artifacts хранятся 14 дней; постоянные файлы релиза находятся в Package Registry.

API загрузки файла:

```text
https://gitlab.campbox.tech/api/v4/projects/<project-id>/packages/generic/content-cache/1.4.0-campbox.1/content-cache_linux_amd64
```

Документация: [Generic Package Registry](https://docs.gitlab.com/user/packages/generic_packages/),
[GitLab Releases](https://docs.gitlab.com/api/releases/).

## Установка на сервер

Сервис уже работает на `root@109.172.97.212`. Установщик обновляет бинарник
и перезапускает существующий unit `content-cache.service`; запуск Go на сервере
не требуется. Unit и пример настроек находятся в [deploy/](deploy/).
Они используют текущий адрес `172.17.0.1:4874`, каталог `/var/lib/content-cache`,
ограничения MemoryMax=4G/CPUQuota=300% и не задают NO_PROXY.
Для изменения лимита создать `/etc/content-cache/content-cache.env` на основе
`deploy/content-cache.env.example` и перезапустить сервис.

Создать deploy token проекта с правом `read_package_registry`. Передать его
установщику через `CONTENT_CACHE_DEPLOY_TOKEN`, не через аргументы командной строки.
Для ввода токена в bash:

```sh
read -rs -p 'GitLab deploy token: ' CONTENT_CACHE_DEPLOY_TOKEN; echo
export CONTENT_CACHE_DEPLOY_TOKEN
python3 scripts/install-release.py --project tlcamp/content-cache \
  --version 1.4.0-campbox.1 --dry-run
python3 scripts/install-release.py --project tlcamp/content-cache \
  --version 1.4.0-campbox.1
unset CONTENT_CACHE_DEPLOY_TOKEN
```

Вторую команду выполнять на сервере от root. Установщик определяет архитектуру,
скачивает бинарник и SHA256SUMS через HTTPS, проверяет SHA256 и `--version`.
`--dry-run` выполняет загрузку и проверки без изменения сервиса.
Обновление блокируется файловым lock, останавливает сервис, сохраняет прежний
бинарник и metadata.db, атомарно заменяет бинарник, запускает сервис и проверяет
`/health`. Если запуск неуспешен, восстанавливает предыдущие бинарник и metadata.db.
Архивы пакетов и настройки systemd сохраняются. Резервные копии:
`/var/lib/content-cache/recovery/releases/<timestamp>/`.

Установщик рассчитан на текущий локальный файловый Package Registry GitLab.
Переадресация на другой домен запрещена, чтобы deploy token не уходил туда.
При настройке object storage GitLab потребуется отдельно поддержать загрузку
по подписанным URL без передачи токена.

Для нового хоста сначала создать системного пользователя `content-cache`, каталог
`/var/lib/content-cache` с его владельцем и установить unit из `deploy/`.
Затем выполнить `systemctl daemon-reload`, `systemctl enable content-cache` и
запустить установщик релиза. Docker должен быть запущен и иметь bridge `172.17.0.1`.

## Проверка после установки

Дважды запросить крупные метаданные, затем посмотреть записи `cache_result`:

```sh
curl --noproxy '*' -fsS -o /dev/null 'http://172.17.0.1:4874/npm/@types%2Fnode'
curl --noproxy '*' -fsS -o /dev/null 'http://172.17.0.1:4874/npm/@types%2Fnode'
journalctl -u content-cache --since '2 minutes ago' --no-pager
```

Метаданные сохраняются до выдачи ответа: после первого успешного сохранения
следующий запрос должен получить `hit`.

Сравнение сторонних форков и кандидаты для следующих улучшений: [FORKS.md](FORKS.md).
