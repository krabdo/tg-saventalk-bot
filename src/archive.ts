import type { JsonObject, Message } from './types.js';
export interface SendPart { method: string; body: JsonObject; media?: boolean }
export function archiveParts(message: Message, owner: string, edited: boolean): SendPart[] {
  const sender = message.sender_business_bot ? 'AI / Bot' : String(message.from?.id) === owner ? '我' : '对方';
  const header = `${edited ? '✏️ 编辑版本' : '💬 消息'} · ${sender}\n${new Date((message.edit_date ?? message.date) * 1000).toISOString()} · 原消息 #${message.message_id}${message.media_group_id ? `\n相册 ${message.media_group_id}` : ''}`;
  const parts: SendPart[] = [{ method: 'sendMessage', body: { text: header } }];
  if (message.text) {
    parts.push({ method: 'sendMessage', body: { text: message.text, entities: message.entities } });
    return parts;
  }
  const media = [
    ['photo', 'sendPhoto', message.photo?.at(-1)], ['video', 'sendVideo', message.video],
    ['document', 'sendDocument', message.document], ['voice', 'sendVoice', message.voice],
    ['audio', 'sendAudio', message.audio], ['sticker', 'sendSticker', message.sticker],
    ['video_note', 'sendVideoNote', message.video_note], ['animation', 'sendAnimation', message.animation],
  ] as const;
  const found = media.find(([, , file]) => file?.file_id);
  if (found && !message.has_protected_content) {
    const [field, method, file] = found;
    const body: JsonObject = { [field]: file!.file_id };
    if (field !== 'sticker' && field !== 'video_note') {
      body.caption = message.caption;
      body.caption_entities = message.caption_entities;
    }
    if (field === 'photo' || field === 'video' || field === 'animation') body.has_spoiler = message.has_media_spoiler;
    parts.push({ method, body, media: true });
  } else {
    const keys = Object.keys(message).filter(key => !['chat', 'from', 'date', 'message_id', 'business_connection_id', 'entities'].includes(key));
    parts.push({ method: 'sendMessage', body: { text: `⚠️ 媒体未备份（${message.has_protected_content ? '受保护内容' : '不支持的消息类型'}）\n类型字段：${keys.join(', ')}${message.caption ? `\n说明：${message.caption}` : ''}` } });
  }
  return parts;
}
