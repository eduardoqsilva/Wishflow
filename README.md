# Wishflow

Automatiza o fechamento da wishlist do [Tome](https://github.com/eduodev/tome): observa a
wishlist configurada, localiza e baixa cada livro no **Z-Library**, faz o upload de volta no
Tome e **marca a wish como `fulfilled`**, fechando o ciclo por completo.

## Como funciona

```
┌─────────┐   wishlist (open)   ┌──────────┐   busca/baixa   ┌────────────┐
│  Tome   │ ◄────────────────── │ Wishflow │ ──────────────► │ Z-Library  │
│ (API)   │                     └────┬─────┘                 └────────────┘
└─────────┘                          │
     ▲                              │ 1. busca o livro (título + autor)
     │  3. marca a wish como        │ 2. escolhe o melhor formato
     │     fulfilled (admin)        │
     │  4. upload do arquivo ▼      │
     └──────────────────────────────┘
```

1. **Lista** as wishes `open` no Tome.
2. Para cada wish, **busca** no Z-Library pelo título + autor.
3. **Escolhe** a melhor correspondência, respeitando a preferência de formato (e formatos
   absolutos, ex. cbz/cbr para mangás).
4. **Baixa** o arquivo (se ainda não existir em disco).
5. **Envia** o arquivo para o Tome (com `book_type_id`).
6. **Marca** as wishes correspondentes como `fulfilled` via endpoint admin
   (`POST /api/admin/wishlist/{id}/fulfill`), fechando o ciclo.
7. Repete a cada `POLL_INTERVAL`.

### Resiliência contra bot checks (DiamWall/Cloudflare)

Muitos espelhos do Z-Library ficam atrás de uma verificação anti-bot que bloqueia acesso
automatizado. Em vez de burlar essa proteção, o Wishflow faz o mesmo que o plugin
[zlibrary.koplugin](https://github.com/ZlibraryKO/zlibrary.koplugin):

- Detecta a página de bot check (marcadores como `DiamWall`, `Verifying your browser`, etc.)
  em qualquer resposta da API.
- **Descobre automaticamente** outro mirror que responde, fazendo health-check
  (`GET /eapi/info/ok`) em paralelo sobre uma lista de espelhos candidatos.
- Mantém um cache de espelhos bloqueados (TTL ~6 meses) para não insistir neles.
- A lista de espelhos é composta por: `ZLIB_BASE_URL` + `ZLIB_MIRRORS` + lista embutida +
  **lista dinâmica** baixada do servidor de domínios do Z-Library.

## Requisitos

- **Go 1.26+** (para rodar localmente a partir do código)
- Uma instância do **Tome** rodando e acessível
- Uma conta do **Z-Library** com credenciais válidas

## Configuração

1. Clone o repositório e entre na pasta:

   ```bash
   git clone https://github.com/<voce>/wishflow.git
   cd wishflow
   ```

2. Crie seu arquivo de ambiente a partir do exemplo:

   ```bash
   cp .env.example .env
   ```

3. Edite o `.env` com seus dados:

   ```bash
   # --- Tome ---
   TOME_URL=https://library.seu-servidor.com.br   # URL da instancia Tome
   TOME_API_TOKEN=tome_xxxxx                       # token com permissao de admin

   # --- Z-Library ---
   ZLIB_BASE_URL=https://article.sk                # espelho preferido (opcional)
   ZLIB_EMAIL=seu_email@exemplo.com
   ZLIB_PASSWORD=sua_senha
   ```

   > **Nota**: o token do Tome precisa de **permissão de admin** para marcar as wishes
   > como `fulfilled`. Sem admin, o upload funciona, mas a etapa final falhará.

## Como rodar

### Local (Go instalado)

```bash
go run .
```

### Compilar e rodar o binário

```bash
go build -o wishflow .
./wishflow          # Linux/macOS
.\wishflow.exe      # Windows
```

### Docker

```bash
docker compose up -d --build
```

O `docker-compose.yml` monta a pasta `./downloads` como volume, então os arquivos baixados
ficam persistidos na máquina host durante a execução.

## Variáveis de ambiente

### Tome

| Variável | Obrigatório | Padrão | Descrição |
|----------|-------------|--------|-----------|
| `TOME_URL` | sim | — | URL da instância Tome (sem barra final) |
| `TOME_API_TOKEN` | sim | — | Token de API com permissão de admin |
| `TOME_POLL_INTERVAL` | não | `60` | Segundos entre cada checagem da wishlist |
| `TOME_WISHLIST_STATUS` | não | `open` | Status a processar: `open`, `fulfilled`, `dismissed` |
| `TOME_BOOK_TYPE_SLUG` | não | `wishlist` | Slug do tipo de livro usado nos uploads (prioridade sobre o label) |
| `TOME_BOOK_TYPE_LABEL` | não | `Wishlist` | Label do tipo de livro |
| `TOME_UPLOAD_RETRY_INTERVAL` | não | `60` | Segundos entre tentativas de upload |
| `TOME_UPLOAD_MAX_RETRIES` | não | `3` | Máximo de tentativas de upload |

> **Sobre o tipo de livro (book type):** o `TOME_BOOK_TYPE_SLUG` tem **prioridade**. O
> programa primeiro procura um tipo existente com esse slug exato; se não encontrar, procura
> por um label equivalente (case-insensitive); se ainda assim não achar, **cria** um novo tipo
> com o slug e label declarados. Dica: use o slug do tipo que você já usa no Tome (ex.
> `wishlist-imported`) para apontar os uploads para ele, em vez de depender do formato do label.

### Z-Library

| Variável | Obrigatório | Padrão | Descrição |
|----------|-------------|--------|-----------|
| `ZLIB_BASE_URL` | não | — | Espelho preferido (sem barra final) |
| `ZLIB_MIRRORS` | não | — | Espelhos adicionais, separados por vírgula |
| `ZLIB_EMAIL` | sim\* | — | Email da conta Z-Library |
| `ZLIB_PASSWORD` | sim\* | — | Senha da conta Z-Library |
| `ZLIB_LANGUAGES` | não | `portuguese,english` | Idiomas da busca |
| `ZLIB_ORDER` | não | `bestmatch` | Ordenação: `popular`, `bestmatch`, `date`, `titleA`, `title`, `year`, `filesize`, `filesizeA` |
| `ZLIB_FORMAT_PREFERENCE` | não | `epub,mobi,azw3,pdf` | Ranking de formato, do melhor para o pior |
| `ZLIB_ABSOLUTE_FORMATS` | não | `cbz,cbr` | Formatos de prioridade absoluta |
| `ZLIB_DOWNLOAD_DIR` | não | `./downloads` | Diretório temporário dos downloads |
| `ZLIB_MAX_DOWNLOADS_PER_RUN` | não | `1` | Quantos livros baixar por ciclo |

\* Se `ZLIB_EMAIL`/`ZLIB_PASSWORD` estiverem **vazios**, o programa roda em **modo
so-listagem**: apenas lista as wishes do Tome, sem buscar baixar nada.

## Exemplo de saída

```
tome-wishlist-downloader iniciado - Tome=https://library.eduodev.com.br, intervalo=1m0s
zlib: 27 mirror(es) acessiveis encontrados
zlib: autenticado em https://article.sk
tome: tipo de livro "wishlist-imported" (id=5)
[2026-08-30 22:32:29] wishlist (1 itens)
  #4 [open] O pequeno príncipe (Original) (autor: Antoine de Saint-Exupéry)
zlib: downloads restantes hoje: 9223372036854775807
wish #4: buscando "O pequeno príncipe (Original) Antoine de Saint-Exupéry" no zlib...
wish #4: match "O pequeno príncipe (Original)" (epub)
wish #4: baixado para downloads/O pequeno príncipe Original.epub
wish #4: upload ok (book id=39) e arquivo local removido
wish #4: marcada como fulfilled (book id=39) no Tome
```

## Estrutura do projeto

```
wishflow/
├── main.go                    # Orquestração do fluxo e ciclo de polling
├── internal/
│   ├── config/config.go       # Leitura do .env
│   ├── tome/tome.go           # Cliente da API do Tome (wishlist, upload, fulfill)
│   └── zlib/
│       ├── zlib.go            # Cliente da API do Z-Library (login, busca, download)
│       └── mirrors.go         # Descoberta de mirrors e detecção de bot check
├── .env.example               # Modelo de configuração
└── Dockerfile / docker-compose.yml
```

## Licença

Distribuído sob a licença MIT.
