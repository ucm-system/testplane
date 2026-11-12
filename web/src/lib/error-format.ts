/** 错误横幅文案：服务端 `error.message` 常带 `CODE: ` 前缀，与并排的错误码 chip 重复 —— 显示前剥掉。 */
export function stripErrorCodePrefix(code: string, message: string): string {
  return message.startsWith(`${code}: `) ? message.slice(code.length + 2) : message
}
