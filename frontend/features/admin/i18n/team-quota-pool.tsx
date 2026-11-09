export const teamQuotaPoolTranslations: Record<"en" | "ja" | "ru", Record<string, string>> = {
  en: {
    "额度池分配模式": "Pool allocation mode",
    "开启动态后，后台按近期用量把组池份额自动分配给组内各 Key。": "When dynamic, a background job automatically splits the team pool across member keys from recent usage.",
    "组池日 Token 上限": "Pool daily token limit",
    "组内所有 Key 的日 Token 消耗总和不得超过该值；留空表示不启用。": "Combined daily token consumption of all keys in the team can never exceed this value; leave empty to disable.",
    "组池月 Token 上限": "Pool monthly token limit",
    "组池日费用上限（USD）": "Pool daily cost limit (USD)",
    "组池月费用上限（USD）": "Pool monthly cost limit (USD)",
  },
  ja: {
    "额度池分配模式": "プール割当モード",
    "开启动态后，后台按近期用量把组池份额自动分配给组内各 Key。": "動的モードでは、直近の使用量に応じてチームプールをメンバーキーへ自動配分します。",
    "组池日 Token 上限": "プール日次トークン上限",
    "组内所有 Key 的日 Token 消耗总和不得超过该值；留空表示不启用。": "チーム内すべてのキーの日次トークン消費の合計はこの値を超えません。空欄で無効。",
    "组池月 Token 上限": "プール月次トークン上限",
    "组池日费用上限（USD）": "プール日次費用上限（USD）",
    "组池月费用上限（USD）": "プール月次費用上限（USD）",
  },
  ru: {
    "额度池分配模式": "Режим распределения пула",
    "开启动态后，后台按近期用量把组池份额自动分配给组内各 Key。": "В динамическом режиме фоновая задача автоматически делит пул команды между ключами по недавнему использованию.",
    "组池日 Token 上限": "Дневной лимит токенов пула",
    "组内所有 Key 的日 Token 消耗总和不得超过该值；留空表示不启用。": "Суммарное дневное потребление токенов всех ключей команды не превысит это значение; пусто — выключено.",
    "组池月 Token 上限": "Месячный лимит токенов пула",
    "组池日费用上限（USD）": "Дневной лимит затрат пула (USD)",
    "组池月费用上限（USD）": "Месячный лимит затрат пула (USD)",
  },
};
