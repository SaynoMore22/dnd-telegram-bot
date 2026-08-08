# dnd-telegram-bot

Личный Telegram-бот-справочник по разделам [next.dnd.su](https://next.dnd.su/) (D&D 5e24):

- классы
- виды
- предыстории
- заклинания
- магические предметы
- снаряжение

Пишешь название (RU/EN) или кидаешь голосовое — бот отвечает карточкой из локальной SQLite.

Материалы с сайта предназначены для личного офлайн-доступа. Права на контент Wizards of the Coast / переводы dnd.su принадлежат правообладателям. Репозиторий — **private**.

## Быстрый старт

1. Создай бота у [@BotFather](https://t.me/BotFather), возьми токен.
2. Узнай свой Telegram user id ([@userinfobot](https://t.me/userinfobot)).
3. Скопируй конфиг:

```bash
cp .env.example .env
```

Заполни:

```env
TELEGRAM_BOT_TOKEN=...
ALLOWED_USER_IDS=123456789
# опционально, для голоса:
OPENAI_API_KEY=sk-...
```

Чтобы добавить ещё пользователей позже — допиши id через запятую в `ALLOWED_USER_IDS`.

4. Зависимости и загрузка базы с next.dnd.su (долго, но стабильно, с паузами):

```bash
go mod tidy
go run ./cmd/ingest
```

Можно один раздел:

```bash
go run ./cmd/ingest -only spell
```

5. Запуск бота:

```bash
go run ./cmd/bot
```

## Команды бота

| Команда | Что делает |
|--------|------------|
| `/start` `/help` | справка |
| `/stats` | сколько записей в базе |
| любой текст | поиск по названию |
| голосовое | Whisper → тот же поиск |

Если нашлось несколько похожих карточек — бот пришлёт нумерованный список, ответь номером.

## Структура

```
cmd/bot      — Telegram-бот
cmd/ingest   — загрузчик next.dnd.su → SQLite
internal/
  bot/       — хендлеры, whitelist
  config/    — .env
  format/    — текст для Telegram
  scrape/    — парсер сайта
  store/     — SQLite + FTS5
  voice/     — OpenAI Whisper
```

База лежит в `data/dnd.db` и **не коммитится**.

## GitHub (private)

```bash
cd /g/DEV/dnd-telegram-bot   # или G:\DEV\dnd-telegram-bot
git init
git add .
git commit -m "Initial private D&D 5e24 Telegram reference bot"
gh repo create dnd-telegram-bot --private --source=. --remote=origin --push
```

Если `gh` попросит логин: `gh auth login`.
