# Gekko → TRMNL plugin

Shows today's and this-week's P&L, profit factor, and win rate on a TRMNL e-ink
display. Delivery is **push**: gekko stays fully local, a cron job POSTs metrics
to a TRMNL private-plugin webhook. No inbound exposure or tunnel required.

## Data source

Liveview endpoint `GET /api/trmnl` (added in `gordon/liveview/run.go`,
`handleTRMNL`). It reads `gekko:trades` from Redis, buckets closed trades into
**today** (since local midnight) and **this week** (since Monday 00:00 local),
and returns the merge-variable envelope TRMNL expects:

```json
{
  "merge_variables": {
    "today": { "pnl": "+142.18", "pnl_value": 142.18, "pnl_positive": true,
               "profit_factor": "1.62", "win_rate": "58", "trades": 12 },
    "week":  { "pnl": "+318.44", "pnl_value": 318.44, "pnl_positive": true,
               "profit_factor": "1.48", "win_rate": "55", "trades": 41 },
    "updated": "Jul 13 10:14",
    "timestamp": 1783962840417
  }
}
```

- `profit_factor` is `—` when there are no trades and `∞` when there are wins but
  no losses yet; `win_rate` is `—` when the window is empty.
- The whole payload is already the exact body to POST to TRMNL — no reshaping.

> Requires a liveview rebuild + restart to expose the route (the currently
> running binary predates it). `./run.sh` rebuilds it.

## TRMNL setup (in your usetrmnl.com account)

1. **Plugins → Private Plugin → Add New**. Name it "Gekko", strategy **Webhook**.
2. Save. TRMNL gives you a plugin UUID and a push URL:
   `https://usetrmnl.com/api/custom_plugins/<UUID>`.
3. Paste the markup below into the plugin's **Markup** editor (Full + Half views
   share it fine). Save.
4. Wire up the pusher (next section) with that UUID.

## Markup (Liquid)

E-ink is grayscale, so the P&L sign is shown with ▲/▼ rather than color.

Paste each block into the matching view tab in the TRMNL markup editor. **Full**
is the two-column layout. **Half Vertical** is required for mashups — when the
plugin shares the screen with another, TRMNL renders it into a tall, narrow half
slot and errors ("Half vertical view not available") if that tab is empty. Paste
the Half Horizontal / Quadrant blocks too if you use those layouts.

### Full view

```liquid
<div class="layout">
  <div class="grid grid--cols-2 gap--large">

    <div class="col col--center">
      <span class="label label--gray-3">TODAY</span>
      <div class="value value--xxlarge value--tnums">
        {% if today.pnl_positive %}▲{% else %}▼{% endif %} {{ today.pnl }}
      </div>
      <div class="grid grid--cols-2 gap--medium w--full">
        <div class="item item--center">
          <span class="value value--large value--tnums">{{ today.profit_factor }}</span>
          <span class="label label--small label--gray-3">PROFIT FACTOR</span>
        </div>
        <div class="item item--center">
          <span class="value value--large value--tnums">{{ today.win_rate }}%</span>
          <span class="label label--small label--gray-3">WIN RATE</span>
        </div>
      </div>
      <span class="label label--xsmall label--gray-4">{{ today.trades }} trades</span>
    </div>

    <div class="col col--center">
      <span class="label label--gray-3">THIS WEEK</span>
      <div class="value value--xxlarge value--tnums">
        {% if week.pnl_positive %}▲{% else %}▼{% endif %} {{ week.pnl }}
      </div>
      <div class="grid grid--cols-2 gap--medium w--full">
        <div class="item item--center">
          <span class="value value--large value--tnums">{{ week.profit_factor }}</span>
          <span class="label label--small label--gray-3">PROFIT FACTOR</span>
        </div>
        <div class="item item--center">
          <span class="value value--large value--tnums">{{ week.win_rate }}%</span>
          <span class="label label--small label--gray-3">WIN RATE</span>
        </div>
      </div>
      <span class="label label--xsmall label--gray-4">{{ week.trades }} trades</span>
    </div>

  </div>
</div>

<div class="title_bar">
  <span class="title">Gekko P&amp;L</span>
  <span class="instance">Updated {{ updated }}</span>
</div>
```

### Half Vertical view (mashups)

Tall and narrow, so Today and This Week stack vertically instead of side by side.

This view deliberately avoids TRMNL's `.layout` / `.col` / `<hr>` classes for
positioning — those default to vertical centering + `space-between` and a
flex-grow divider, which pushed the two blocks apart (or one off-screen). Instead
it uses a plain inline-flex column pinned to the top, and keeps the framework
`value` / `label` classes for typography only. Adjust spacing with the two `gap`
values and the top `padding`.

```liquid
<div style="display:flex; flex-direction:column; align-items:center; gap:32px; padding:24px 0;">

  <div style="display:flex; flex-direction:column; align-items:center; gap:2px;">
    <span class="label label--gray-3">TODAY</span>
    <div class="value value--xlarge value--tnums">
      {% if today.pnl_positive %}▲{% else %}▼{% endif %} {{ today.pnl }}
    </div>
    <div style="display:flex; gap:36px; margin-top:4px;">
      <div style="text-align:center;">
        <div class="value value--medium value--tnums">{{ today.profit_factor }}</div>
        <div class="label label--xsmall label--gray-3">PF</div>
      </div>
      <div style="text-align:center;">
        <div class="value value--medium value--tnums">{{ today.win_rate }}%</div>
        <div class="label label--xsmall label--gray-3">WIN</div>
      </div>
    </div>
    <span class="label label--xsmall label--gray-4">{{ today.trades }} trades</span>
  </div>

  <div style="width:70%; border-top:2px solid currentColor; opacity:0.5;"></div>

  <div style="display:flex; flex-direction:column; align-items:center; gap:2px;">
    <span class="label label--gray-3">THIS WEEK</span>
    <div class="value value--xlarge value--tnums">
      {% if week.pnl_positive %}▲{% else %}▼{% endif %} {{ week.pnl }}
    </div>
    <div style="display:flex; gap:36px; margin-top:4px;">
      <div style="text-align:center;">
        <div class="value value--medium value--tnums">{{ week.profit_factor }}</div>
        <div class="label label--xsmall label--gray-3">PF</div>
      </div>
      <div style="text-align:center;">
        <div class="value value--medium value--tnums">{{ week.win_rate }}%</div>
        <div class="label label--xsmall label--gray-3">WIN</div>
      </div>
    </div>
    <span class="label label--xsmall label--gray-4">{{ week.trades }} trades</span>
  </div>

</div>

<div class="title_bar">
  <span class="title">Gekko P&amp;L</span>
  <span class="instance">{{ updated }}</span>
</div>
```

### Half Horizontal view (mashups)

Wide but short. Today and This Week stay side by side (like Full) but compressed
vertically — smaller values, trimmed labels.

```liquid
<div class="layout">
  <div class="grid grid--cols-2 gap--large">

    <div class="col col--center">
      <span class="label label--small label--gray-3">TODAY</span>
      <div class="value value--large value--tnums">
        {% if today.pnl_positive %}▲{% else %}▼{% endif %} {{ today.pnl }}
      </div>
      <div class="grid grid--cols-2 gap--small w--full">
        <div class="item item--center">
          <span class="value value--medium value--tnums">{{ today.profit_factor }}</span>
          <span class="label label--xsmall label--gray-3">PF</span>
        </div>
        <div class="item item--center">
          <span class="value value--medium value--tnums">{{ today.win_rate }}%</span>
          <span class="label label--xsmall label--gray-3">WIN</span>
        </div>
      </div>
    </div>

    <div class="col col--center">
      <span class="label label--small label--gray-3">THIS WEEK</span>
      <div class="value value--large value--tnums">
        {% if week.pnl_positive %}▲{% else %}▼{% endif %} {{ week.pnl }}
      </div>
      <div class="grid grid--cols-2 gap--small w--full">
        <div class="item item--center">
          <span class="value value--medium value--tnums">{{ week.profit_factor }}</span>
          <span class="label label--xsmall label--gray-3">PF</span>
        </div>
        <div class="item item--center">
          <span class="value value--medium value--tnums">{{ week.win_rate }}%</span>
          <span class="label label--xsmall label--gray-3">WIN</span>
        </div>
      </div>
    </div>

  </div>
</div>

<div class="title_bar">
  <span class="title">Gekko P&amp;L</span>
  <span class="instance">{{ updated }}</span>
</div>
```

### Quadrant view (mashups)

A quarter of the screen — very little room. Two stacked rows, one P&L each for
Today and This Week, with win rate as a small suffix. Same inline-flex approach
as Half Vertical so both rows actually render.

```liquid
<div style="display:flex; flex-direction:column; align-items:center; gap:14px; padding:8px 0;">

  <div style="display:flex; flex-direction:column; align-items:center; gap:1px;">
    <span class="label label--xsmall label--gray-3">TODAY · {{ today.win_rate }}% WIN</span>
    <div class="value value--large value--tnums">
      {% if today.pnl_positive %}▲{% else %}▼{% endif %} {{ today.pnl }}
    </div>
  </div>

  <div style="display:flex; flex-direction:column; align-items:center; gap:1px;">
    <span class="label label--xsmall label--gray-3">THIS WEEK · {{ week.win_rate }}% WIN</span>
    <div class="value value--large value--tnums">
      {% if week.pnl_positive %}▲{% else %}▼{% endif %} {{ week.pnl }}
    </div>
  </div>

</div>
```

## Pusher (Step 3 — cron)

Built: [`scripts/trmnl_push.sh`](../scripts/trmnl_push.sh). Pulls `/api/trmnl`,
guards against an empty/HTML response, and POSTs the body to the plugin webhook.
UUID comes from `$TRMNL_UUID` or the untracked `trmnl_plugin_uuid.txt`.

Cron (every 15 min — matches TRMNL free-tier refresh):

```
*/15 * * * * /path/to/gekko/scripts/trmnl_push.sh >/dev/null 2>&1
```
