# imports — CSV/OFX Statement Import

Synchronous parse → dedup → map pipeline. See `ARCHITECTURE.md` §8.6.

## Entry points (`handler.go`)

`Handler{pool, log}` + `NewHandler(pool, cfg, log)`.

`StartImport(ctx, {accountID, fileName, fileFormat, content})` → `{job_id, total_rows}` at `POST /actions/import/start`. Job lifecycle: `pending → parsing → mapping → completed/failed`.

## Files

| File | Responsibility |
|---|---|
| `parser_csv.go` | `ParseCSV` — BOM strip, header-driven or positional `date,amount,description` |
| `parser_ofx.go` | `ParseOFX` — SGML scan of `<STMTTRN>`, `CURDEF`/`DTPOSTED`/`TRNAMT`/`NAME`+`MEMO`/`FITID` |
| `dedup.go` | `AssignOccurrences`, `DedupHash`, `IsDuplicate` (SHA256) |
| `mapper.go` | `DecimalToMinor`, `MapRow` — amount parsing, FX, ledger + balance |
| `handler.go` | `runPipeline`, `mapOne`, `parseContent` orchestration |

`ParsedRow{occurred_at, amount (decimal text), currency?, description?, fitid?}`.

## Tables

`import.import_jobs` (status, `total_rows`, `imported_rows`, `duplicate_rows`, `error_message`, `completed_at`), `import.raw_rows` (`row_number`, `raw_data::jsonb`, `dedup_hash`, `status ∈ {pending, mapped, duplicate, error}`, `mapped_transaction_id`), plus `tx.transactions` (`source=import`), `account.balance_ledger`, `account.accounts`.

## Rules

- Raw-row insert in one `WithSerializable` tx; each row mapped in its own `WithSerializable` tx so one bad row (`status=error`) doesn't fail the job.
- Dedup hash: `SHA256(account|FITID)` when the file carries a bank id, else `SHA256(account|date|amount|desc|occurrence_n)` where `occurrence_n` is the ordinal of the identical row in the file. `UNIQUE(import_job_id, dedup_hash)` guards within a job only; cross-job duplicates found via `IsDuplicate` lookup against earlier `mapped` rows.
- `DecimalToMinor`: strips spaces/NBSP, last of `,.` is the decimal separator (`1,234.56`, `1.234,56`, `1 234,56` all work); `big.Rat × 10^minor_unit`, truncated; rejects invalid/overflow/rounds-to-zero.
- `MapRow`: zero amount rejected; sign → kind (`+` income, `−` expense, abs stored); row currency if known else account currency; cross-currency via nearest `fx.rates` + `common.ConvertToMinor`; account locked `FOR UPDATE` before ledger + balance update.
- CSV dates accept RFC3339, `2006-01-02[ 15:04:05]`, `20060102`, `02.01.2006`, `02/01/2006`, `01/02/2006` → UTC. OFX `DTPOSTED` uses first 8 chars `YYYYMMDD`.

## Errors

`BadRequest` (bad format, empty name/content, parser/date/amount failures), `NotFound` (account incl. cross-org, missing FX rate), `Wrap` for DB failures.

## Tests

`imports_test.go` — CSV header/positional parsing, amountless-row skip, OFX flat-tag parsing, `DecimalToMinor` locales, dedup occurrence distinction. Pure functions only.
