# Исследование сжатия изображений: TinyPNG vs алгоритм на Go

_Сгенерировано: 24.11.2026 00:25, инструмент: `research/tool` (чистый Go, stdlib)._

Методика: каждая пара «оригинал → результат TinyPNG» разбирается на уровне формата
(PNG-чанки, JPEG-маркеры, квант-таблицы, фильтры строк), считаются метрики качества
(PSNR по RGB поверх белого, SSIM по яркости в окнах 8×8, уникальные цвета),
затем оригиналы прожариваются собственной Go-реализацией (median-cut квантизация +
опциональный Floyd–Steinberg + BestCompression) и сравниваются с TinyPNG по размеру и качеству.

## 1. Что именно делает TinyPNG с файлами

### PNG

| Файл | Цвет. модель до → после | Палитра (записей) | Метаданные | IDAT сжатие | Фильтры строк (N/S/U/A/P) |
|------|------------------------|-------------------|------------|-------------|---------------------------|
| Dashboard UI Elements schedule graph statistics card clean website ui.png | truecolor+alpha → indexed (palette) | 255 | нет | 13.55% | 1200/0/0/0/0 |
| PV_ART_Hand.png | truecolor+alpha → indexed (palette) | 256 | удалены (1) | 9.32% | 2500/0/0/0/0 |
| PV_ART_Nyasha_sleep.png | truecolor+alpha → indexed (palette) | 256 | удалены (1) | 10.77% | 2500/0/0/0/0 |
| WonderKids Landing Page Exploration by Paperpillar on Dribbble.png | truecolor+alpha → indexed (palette) | 256 | нет | 8.76% | 2400/0/0/0/0 |
| catgirl_hoodie_halfzipped_small_chest.png | truecolor RGB → indexed (palette) | 256 | удалены (1) | 36.84% | 1536/0/0/0/0 |

Чанки TinyPNG-версии по файлам:

- `Dashboard UI Elements schedule graph statistics card clean website ui.png`: IHDR(13), PLTE(765), IDAT(260336), IEND(0)
- `PV_ART_Hand.png`: IHDR(13), PLTE(768), IDAT(524287), IDAT(512168), IEND(0)
- `PV_ART_Nyasha_sleep.png`: IHDR(13), PLTE(768), IDAT(524287), IDAT(524287), IDAT(148331), IEND(0)
- `WonderKids Landing Page Exploration by Paperpillar on Dribbble.png`: IHDR(13), PLTE(768), IDAT(524287), IDAT(148926), IEND(0)
- `catgirl_hoodie_halfzipped_small_chest.png`: IHDR(13), PLTE(768), IDAT(524287), IDAT(55798), IEND(0)

### JPEG

| Файл | Режим | Субдискретизация | ~качество (по квант-таблицам) | Маркеры после |
|------|-------|------------------|-------------------------------|---------------|
| Components Web App Interface by Golo on Dribbble.jpg | progressive | 4:4:4 | 27 | APP0/JFIF, DQT, DQT, SOF2 progressive, DHT, DHT, SOS |
| photo_2026-11-06 20.37.22.jpeg | progressive | 4:2:0 | 29 | APP0/JFIF, DQT, DQT, SOF2 progressive, DHT, DHT, SOS |

## 2. Размеры и качество результата TinyPNG

| Файл | Было | Стало | Экономия | PSNR | SSIM | Уник. цветов до → после |
|------|------|-------|----------|------|------|-------------------------|
| Components Web App Interface by Golo on Dribbble.jpg | 0.26 МБ | 99.7 КБ | −62.7% | 32.17 дБ | 0.9939 | 52063 → 66265 |
| Dashboard UI Elements schedule graph statistics card clean website ui.png | 0.69 МБ | 255.0 КБ | −63.8% | 45.86 дБ | 0.9988 | 16771 → 255 |
| PV_ART_Hand.png | 2.83 МБ | 1013.0 КБ | −65.1% | 39.89 дБ | 0.9910 | 88338 → 256 |
| PV_ART_Nyasha_sleep.png | 3.56 МБ | 1169.7 КБ | −67.9% | 36.86 дБ | 0.9842 | 127959 → 256 |
| WonderKids Landing Page Exploration by Paperpillar on Dribbble.png | 2.31 МБ | 658.3 КБ | −72.1% | 39.77 дБ | 0.9966 | 66233 → 256 |
| catgirl_hoodie_halfzipped_small_chest.png | 2.30 МБ | 567.3 КБ | −75.9% | 31.73 дБ | 0.9646 | 381375 → 256 |
| photo_2026-11-06 20.37.22.jpeg | 0.11 МБ | 97.1 КБ | −13.5% | 35.36 дБ | 0.9914 | 77489 → 81817 |

## 3. Наш алгоритм на Go против TinyPNG

Реализация: median-cut квантизация палитры (256/128/64 цветов),
дизеринг Флойда–Стейнберга (опционально), палитровый PNG, BestCompression, без метаданных.

### Dashboard UI Elements schedule graph statistics card clean website ui.png (TinyPNG: 255.0 КБ, PSNR 45.86 дБ)

| Вариант | Размер | Δ к TinyPNG | PSNR | SSIM |
|---------|--------|-------------|------|------|
| median-cut 256 + zlib-best + FS-dither | 316.2 КБ | +24.0% | 30.70 дБ | 0.9500 |
| median-cut 256 + zlib-best (без дизеринга) | 158.1 КБ | -38.0% | 40.35 дБ | 1.0000 |
| median-cut 128 + zlib-best + FS-dither | 295.6 КБ | +15.9% | 29.23 дБ | 0.9500 |
| median-cut 128 + zlib-best (без дизеринга) | 145.2 КБ | -43.1% | 38.17 дБ | 0.9900 |
| median-cut 64 + zlib-best + FS-dither | 285.3 КБ | +11.9% | 27.23 дБ | 0.9400 |
| median-cut 64 + zlib-best (без дизеринга) | 136.6 КБ | -46.4% | 34.05 дБ | 0.9900 |

_Лучший по критерию «размер при PSNR не хуже TinyPNG»: **median-cut 256 + zlib-best + FS-dither**._

### PV_ART_Hand.png (TinyPNG: 1013.0 КБ, PSNR 39.89 дБ)

| Вариант | Размер | Δ к TinyPNG | PSNR | SSIM |
|---------|--------|-------------|------|------|
| median-cut 256 + zlib-best + FS-dither | 2453.8 КБ | +142.2% | 27.52 дБ | 0.8300 |
| median-cut 256 + zlib-best (без дизеринга) | 442.3 КБ | -56.3% | 37.79 дБ | 0.9900 |
| median-cut 128 + zlib-best + FS-dither | 2293.8 КБ | +126.4% | 26.68 дБ | 0.8100 |
| median-cut 128 + zlib-best (без дизеринга) | 370.2 КБ | -63.5% | 36.38 дБ | 0.9900 |
| median-cut 64 + zlib-best + FS-dither | 1983.5 КБ | +95.8% | 24.79 дБ | 0.7600 |
| median-cut 64 + zlib-best (без дизеринга) | 270.9 КБ | -73.3% | 32.62 дБ | 0.9800 |

_Лучший по критерию «размер при PSNR не хуже TinyPNG»: **median-cut 256 + zlib-best + FS-dither**._

### PV_ART_Nyasha_sleep.png (TinyPNG: 1169.7 КБ, PSNR 36.86 дБ)

| Вариант | Размер | Δ к TinyPNG | PSNR | SSIM |
|---------|--------|-------------|------|------|
| median-cut 256 + zlib-best + FS-dither | 2628.8 КБ | +124.7% | 26.10 дБ | 0.8600 |
| median-cut 256 + zlib-best (без дизеринга) | 584.7 КБ | -50.0% | 32.35 дБ | 0.9800 |
| median-cut 128 + zlib-best + FS-dither | 2413.8 КБ | +106.4% | 24.76 дБ | 0.8400 |
| median-cut 128 + zlib-best (без дизеринга) | 489.7 КБ | -58.1% | 31.00 дБ | 0.9800 |
| median-cut 64 + zlib-best + FS-dither | 2127.3 КБ | +81.9% | 22.97 дБ | 0.8100 |
| median-cut 64 + zlib-best (без дизеринга) | 349.6 КБ | -70.1% | 29.45 дБ | 0.9700 |

_Лучший по критерию «размер при PSNR не хуже TinyPNG»: **median-cut 256 + zlib-best + FS-dither**._

### WonderKids Landing Page Exploration by Paperpillar on Dribbble.png (TinyPNG: 658.3 КБ, PSNR 39.77 дБ)

| Вариант | Размер | Δ к TinyPNG | PSNR | SSIM |
|---------|--------|-------------|------|------|
| median-cut 256 + zlib-best + FS-dither | 993.8 КБ | +51.0% | 29.85 дБ | 0.9500 |
| median-cut 256 + zlib-best (без дизеринга) | 506.2 КБ | -23.1% | 34.91 дБ | 0.9900 |
| median-cut 128 + zlib-best + FS-dither | 938.8 КБ | +42.6% | 27.26 дБ | 0.9400 |
| median-cut 128 + zlib-best (без дизеринга) | 458.7 КБ | -30.3% | 32.14 дБ | 0.9900 |
| median-cut 64 + zlib-best + FS-dither | 856.9 КБ | +30.2% | 24.49 дБ | 0.9100 |
| median-cut 64 + zlib-best (без дизеринга) | 403.1 КБ | -38.8% | 29.65 дБ | 0.9900 |

_Лучший по критерию «размер при PSNR не хуже TinyPNG»: **median-cut 256 + zlib-best + FS-dither**._

### catgirl_hoodie_halfzipped_small_chest.png (TinyPNG: 567.3 КБ, PSNR 31.73 дБ)

| Вариант | Размер | Δ к TinyPNG | PSNR | SSIM |
|---------|--------|-------------|------|------|
| median-cut 256 + zlib-best + FS-dither | 641.1 КБ | +13.0% | 22.90 дБ | 0.8100 |
| median-cut 256 + zlib-best (без дизеринга) | 448.5 КБ | -21.0% | 29.12 дБ | 0.9600 |
| median-cut 128 + zlib-best + FS-dither | 542.0 КБ | -4.5% | 20.70 дБ | 0.7400 |
| median-cut 128 + zlib-best (без дизеринга) | 342.9 КБ | -39.6% | 26.95 дБ | 0.9300 |
| median-cut 64 + zlib-best + FS-dither | 436.6 КБ | -23.0% | 17.62 дБ | 0.6300 |
| median-cut 64 + zlib-best (без дизеринга) | 266.1 КБ | -53.1% | 23.78 дБ | 0.8900 |

_Лучший по критерию «размер при PSNR не хуже TinyPNG»: **median-cut 256 + zlib-best + FS-dither**._

### Components Web App Interface by Golo on Dribbble.jpg (JPEG, TinyPNG: 99.7 КБ)

Прогон качества stdlib-энкодера (baseline, без mozjpeg); для изображений >8 Мп метрики по подвыборке 2×:

| Вариант | Размер | Δ к TinyPNG | PSNR | SSIM |
|---------|--------|-------------|------|------|
| image/jpeg q=50 | 91.4 КБ | -8.4% | 29.52 дБ | 0.9900 |
| image/jpeg q=60 | 99.8 КБ | +0.1% | 30.12 дБ | 0.9900 |
| image/jpeg q=65 | 106.7 КБ | +7.0% | 30.51 дБ | 0.9900 |
| image/jpeg q=70 | 113.6 КБ | +14.0% | 31.00 дБ | 1.0000 |
| image/jpeg q=75 | 123.1 КБ | +23.5% | 31.43 дБ | 1.0000 |
| image/jpeg q=80 | 137.4 КБ | +37.8% | 32.17 дБ | 1.0000 |
| image/jpeg q=85 | 152.5 КБ | +52.9% | 32.85 дБ | 1.0000 |
| image/jpeg q=90 | 185.3 КБ | +85.8% | 33.69 дБ | 1.0000 |
| image/jpeg q=95 | 242.5 КБ | +143.2% | 34.54 дБ | 1.0000 |

### photo_2026-11-06 20.37.22.jpeg (JPEG, TinyPNG: 97.1 КБ)

Прогон качества stdlib-энкодера (baseline, без mozjpeg); для изображений >8 Мп метрики по подвыборке 2×:

| Вариант | Размер | Δ к TinyPNG | PSNR | SSIM |
|---------|--------|-------------|------|------|
| image/jpeg q=50 | 91.6 КБ | -5.7% | 30.36 дБ | 0.9700 |
| image/jpeg q=60 | 101.7 КБ | +4.7% | 34.12 дБ | 0.9900 |
| image/jpeg q=65 | 105.1 КБ | +8.2% | 36.25 дБ | 0.9900 |
| image/jpeg q=70 | 110.2 КБ | +13.4% | 39.44 дБ | 1.0000 |
| image/jpeg q=75 | 113.4 КБ | +16.7% | 72.77 дБ | 1.0000 |
| image/jpeg q=80 | 119.8 КБ | +23.4% | 40.93 дБ | 1.0000 |
| image/jpeg q=85 | 146.0 КБ | +50.2% | 42.45 дБ | 1.0000 |
| image/jpeg q=90 | 161.8 КБ | +66.5% | 44.15 дБ | 1.0000 |
| image/jpeg q=95 | 212.5 КБ | +118.8% | 49.48 дБ | 1.0000 |

## 4. Гейты: наш компрессор против TinyPNG

Критерии (Q8/Q11): размер ≤ +20% от TinyPNG; SSIM ≥ TinyPNG − 0.02.

| Файл | Наш размер | Δ к TinyPNG | Наш PSNR | Наш SSIM | TinyPNG SSIM | Гейт размера | Гейт SSIM |
|------|------------|-------------|----------|----------|--------------|--------------|-----------|
| Components Web App Interface by Golo on Dribbble.jpg | 93.2 КБ | -6.6% | 29.64 дБ | 0.9930 | 0.9939 | ✓ | ✓ |
| Dashboard UI Elements schedule graph statistics card clean website ui.png | 217.1 КБ | -14.9% | 39.43 дБ | 0.9861 | 0.9988 | ✓ | ✓ |
| PV_ART_Hand.png | 1142.6 КБ | +12.8% | 37.57 дБ | 0.9850 | 0.9910 | ✓ | ✓ |
| PV_ART_Nyasha_sleep.png | 1382.9 КБ | +18.2% | 33.95 дБ | 0.9805 | 0.9842 | ✓ | ✓ |
| WonderKids Landing Page Exploration by Paperpillar on Dribbble.png | 672.4 КБ | +2.1% | 36.49 дБ | 0.9911 | 0.9966 | ✓ | ✓ |
| catgirl_hoodie_halfzipped_small_chest.png | 594.1 КБ | +4.7% | 28.98 дБ | 0.9544 | 0.9646 | ✓ | ✓ |
| photo_2026-11-06 20.37.22.jpeg | 97.6 КБ | +0.5% | 31.10 дБ | 0.9787 | 0.9914 | ✓ | ✓ |

**Вердикт: 7/7 пар проходят оба гейта.**

