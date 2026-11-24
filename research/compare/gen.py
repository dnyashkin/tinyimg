#!/usr/bin/env python3
# gen.py — пересборка index.html из report.json (прогон текущего бинаря).
# Стили и JS берутся из существующего index.html, перегенерируются hero и карточки.
import json
import re
import sys
import urllib.parse
from pathlib import Path

here = Path(__file__).parent


def human(n: int) -> str:
    if n >= 1024 * 1024:
        return f"{n / 1024 / 1024:.2f} МБ"
    return f"{n / 1024:.1f} КБ"


def main() -> None:
    raw = (here / "report.json").read_text(encoding="utf-8")
    report = json.JSONDecoder().raw_decode(raw)[0]
    html = (here / "index.html").read_text(encoding="utf-8")

    prefix, rest = html.split('<div class="hero">', 1)
    _, suffix = rest.split("</main>", 1)

    results = sorted(report["results"], key=lambda r: -r["before"])
    total = report["total"]
    saved = 100 - total["after"] * 100 / total["before"]

    hero = f'''<div class="hero">
  <h1>tinyimg — до / после</h1>
  <p>Корпус <code>research/orginal</code>, {len(results)} файлов. Прогон текущего бинаря, выход детерминирован.</p>
  <p>Тяни разделитель мышью или пальцем. Кнопка «1:1 рядом» — оба изображения в натуральном размере с синхронной прокруткой (так видно дизеринг и ступени без масштабной интерполяции).</p>
  <div class="total">
    <span class="chip chip-before">Было · {human(total["before"])}</span>
    <span class="chip chip-after">Стало · {human(total["after"])} (−{saved:.1f}%)</span>
    <span class="chip">wall {total["wall_ms"] / 1000:.1f} с</span>
  </div>
</div>
<main>
'''

    cards = []
    for r in results:
        name = r["name"]
        url = urllib.parse.quote(name)
        pct = r["saved_percent"]
        if pct > 0:
            after_chip = f"Сжато · {human(r['after'])} (−{pct:.1f}%)"
            lbl_r = f"Сжато −{pct:.1f}%"
        else:
            after_chip = f"Без изменений · {human(r['after'])}"
            lbl_r = "Как оригинал"
        cards.append(f'''  <section class="card">
    <header class="card-head">
      <h2>{name}</h2>
      <div class="meta">
        <span class="chip chip-before">Оригинал · {human(r["before"])}</span>
        <span class="chip chip-after">{after_chip}</span>
        <span class="chip chip-strat">{r["strategy"]}</span>
        <button class="mode-btn" type="button">1:1 рядом</button>
      </div>
    </header>
    <div class="cmp" style="--pos:50%">
      <img class="base" loading="lazy" src="original/{url}" alt="{name} — оригинал" draggable="false">
      <img class="top"  loading="lazy" src="out/{url}" alt="{name} — после сжатия" draggable="false">
      <div class="handle"><span></span></div>
      <span class="lbl lbl-l">Оригинал</span>
      <span class="lbl lbl-r">{lbl_r}</span>
    </div>
    <div class="pair">
      <figure><figcaption>Оригинал · {human(r["before"])}</figcaption><div class="scroll"><img loading="lazy" src="original/{url}" alt="" draggable="false"></div></figure>
      <figure><figcaption>Сжато · {human(r["after"])}</figcaption><div class="scroll"><img loading="lazy" src="out/{url}" alt="" draggable="false"></div></figure>
    </div>
  </section>
''')

    out = prefix + hero + "\n" + "\n".join(cards) + "</main>" + suffix
    (here / "index.html").write_text(out, encoding="utf-8")
    print(f"index.html: {len(results)} карточек, итого −{saved:.1f}%")


if __name__ == "__main__":
    sys.exit(main())
