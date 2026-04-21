# Page Classification

Classify a webpage into one of 7 types based on URL, content signals, and data patterns.

> **Data source**: all classification signals come from ptengine-cli responses
> (page_metrics, block_metrics, URL strings). Do not fetch the live page.

## 7 Page Types

### 0. Sales Landing Page (Direct Response LP)
- **Goal**: Immediate conversion (Purchase or Lead Gen)
- **Key Signals**: "Island" design (no nav menu, no footer links), single goal (all CTAs → same action), long-form PAS structure
- **Storyline**: Hero → Problem/Agitation → Solution → Benefits → Social Proof → Offer/Guarantee → FAQ → Closing CTA
- **Internal key**: `sales_lp` (use `ad_lp` if ad traffic >50%)

### 1. Article LP (Advertorial)
- **Goal**: Education and pre-selling
- **Key Signals**: Blog/news/review layout, text-heavy, soft CTA ("Read More" / link to product page)
- **Storyline**: Headline → Problem Awareness → Cause Analysis → Discovery → Solution (Soft Intro) → Social Proof → Transition CTA
- **Internal key**: `article_lp`

### 2. Product Detail Page (PDP)
- **Goal**: Transactional (Add to Cart / Buy)
- **Key Signals**: E-commerce layout, product gallery + price + specs + "Add to Cart", reviews/UGC
- **Storyline**: Gallery & Buy Box → Cross-sell → Product Details → Feature Deep-dive → Reviews/UGC → Footer CTA
- **Internal key**: `pdp`

### 3. Homepage (Brand Website)
- **Goal**: Traffic hub / Navigation / Branding
- **Key Signals**: Root domain, full navigation bar, mixed content (arrivals, story, blog, collections)
- **Storyline**: Hero Banner → Navigation/Categories → New Arrivals/Best Sellers → Brand Story → Blog → Trust/Footer
- **Internal key**: `homepage`

### 4. Campaign / Promotion Page
- **Goal**: Event-based sales (Black Friday, 11.11)
- **Key Signals**: Poster style, coupons/countdown/discounts, lists multiple products
- **Storyline**: Key Visual → Timeline/Rules → Coupons → Hero Products → Category Floors → Urgency Footer
- **Internal key**: `sales_lp` (uses same analysis builds as sales_lp)

### 5. Other Content
- **Goal**: Information delivery, soft conversion (apply, contact, download, register)
- **Key Signals**: Informational/editorial content, no direct e-commerce purchase flow
- **Examples**: Recruitment, corporate/brand intro, FAQ/help, documentation, gaming, blog, pricing (SaaS)
- **Internal key**: `other_content`

### 6. Other Function
- **Goal**: Task completion (log in, sign up, reset password, checkout)
- **Key Signals**: Forms, input fields, functional link lists, minimal informational content
- **Examples**: Login, registration, password reset, cart, checkout, 404, sitemap
- **Internal key**: `other_function`

## Classification Steps

1. **Check Navigation**: Full menu with many links? → Likely Homepage or PDP
2. **Check Primary Elements**: Forms/inputs → Other Function; Text/content → Other Content or named types
3. **Check CTA type**: "Buy Now" → Sales LP; "Add to Cart" → PDP; "Read More" → Article LP; Apply/Contact → Other Content; Submit/Proceed → Other Function
4. **Check URL Structure**: Root domain → Homepage; /login, /cart, /checkout → Other Function
5. **Check Content Flow**: PAS story → Sales LP / Article LP; Spec listing → PDP; Info/editorial → Other Content
6. **Determine ad_lp vs sales_lp**: If page_insight sourceType shows >50% ad/UTM traffic → use `ad_lp`

## Classification Result

After classification, record these internally (this is an intermediate step — not shown to the user):
- **category_name**: One of the 7 types above
- **internal key**: The mapped key (ad_lp, sales_lp, pdp, homepage, article_lp, other_content, other_function)
- **reason**: Brief rationale for the classification

If uncertain, ask the user to confirm before proceeding.

## Internal Key Mapping

| Category Name | Internal key | Notes |
|---|---|---|
| Sales Landing Page | `sales_lp` or `ad_lp` | Use ad_lp when ad traffic dominant |
| Article LP | `article_lp` | |
| Product Detail Page | `pdp` | |
| Homepage | `homepage` | |
| Campaign / Promotion Page | `sales_lp` | No separate build |
| Other Content | `other_content` | |
| Other Function | `other_function` | |
