// 密钥掩码展示:仅暴露前 5 与后 5 个字符,中间以 **** 隐藏(防窥)。
// 短密钥(≤10 字符)首尾拼接会泄露完整内容,一律整体隐藏。
// 掩码仅供展示;需要完整值请走复制动作。
export function maskKey(key: string): string {
  if (!key) return ''
  if (key.length <= 10) return '****'
  return `${key.slice(0, 5)}****${key.slice(-5)}`
}
