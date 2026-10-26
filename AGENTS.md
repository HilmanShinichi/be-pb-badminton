# AGENTS — backend (Go)

Baca `../README.md` **dulu** sebelum explore. Berisi peta lengkap: stack, route
map, struktur module, migration, dan gotchas. Tujuannya supaya tidak perlu
re-explore dari nol.

## Konteks cepat

- Go 1.26 + GoFiber v2, raw SQL via `pgx/v5`, **tanpa ORM**.
- Route: **semua** di `internal/httpapi/router.go`. Jangan daftarkan route di file lain.
- Module: `internal/modules/<nama>/module.go` (handler + query), di-wire manual
  di `internal/app/app.go`.
- Migrasi: taruh `.sql` di `internal/database/migrations/`, nomor berikutnya
  `0027_...`. Dijalankan otomatis saat boot, urut nama file.
- `.env.example` **wajib** ikut di-update kalau menambah env var.
- Test: `go test ./...`. Test fixture match-maker ada di
  `internal/modules/matchmaker/*_test.go`.

## Wajib setelah mengubah kode

Perbarui `../README.md` sesuai tabel **Maintenance Rules** di bagian bawah file
itu. Ringkasnya:

- Tambah/hapus route → §5 Route Map
- Tambah permission key → §5 (dan samakan `frontend/src/permissions.ts`)
- Migration baru → §6 (tulis nomor berikutnya)
- Ubah alur match-maker / generate / count → §8
- Env var baru → §2 + `.env.example`
- Module baru → §4 + §5

Jangan commit kalau README belum sinkron.
