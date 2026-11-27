import { contextBridge, ipcRenderer } from 'electron'
import type { BridgeEvent, DesktopApi, NoticeClick, OpenedLink, SocketEvent } from '../shared/api.js'

const listeners = new Map<string, (e: BridgeEvent) => void>()
ipcRenderer.on('bridge:event', (_e, id: string, ev: BridgeEvent) => {
  const fn = listeners.get(id)
  if (!fn) return
  if (ev.type === 'end' || ev.type === 'error') listeners.delete(id)
  fn(ev)
})

const sockets = new Map<string, (e: SocketEvent) => void>()
ipcRenderer.on('socket:event', (_e, id: string, ev: SocketEvent) => {
  const fn = sockets.get(id)
  if (!fn) return
  if (ev.type === 'close' || ev.type === 'error') sockets.delete(id)
  fn(ev)
})

let next = 0
const api: DesktopApi = {
  hosts: {
    list: () => ipcRenderer.invoke('hosts:list'),
    add: (h) => ipcRenderer.invoke('hosts:add', h),
    remove: (id) => ipcRenderer.invoke('hosts:remove', id),
    fixed: ipcRenderer.sendSync('hosts:fixed') as boolean,
  },
  openSocket(hostId, path, onEvent) {
    const id = String(++next)
    sockets.set(id, onEvent)
    ipcRenderer.send('socket:open', id, hostId, path)
    return {
      send: (data) => ipcRenderer.send('socket:send', id, data),
      close: () => {
        sockets.delete(id)
        ipcRenderer.send('socket:close', id)
      },
    }
  },
  links: {
    take: () => ipcRenderer.invoke('links:take'),
    onOpen(cb) {
      // Main only rings; the link itself is taken, so it is handed over once.
      const handler = () => void (ipcRenderer.invoke('links:take') as Promise<OpenedLink | undefined>).then((l) => l && cb(l))
      ipcRenderer.on('links:open', handler)
      return () => ipcRenderer.removeListener('links:open', handler)
    },
  },
  local: {
    detect: () => ipcRenderer.invoke('local:detect'),
    connect: (id) => ipcRenderer.invoke('local:connect', id),
    fix: (id, fix) => ipcRenderer.invoke('local:fix', id, fix),
  },
  openExternal: (url) => ipcRenderer.send('shell:openExternal', url),
  shell: {
    notify: (n) => ipcRenderer.send('shell:notify', n),
    setWaiting: (count) => ipcRenderer.send('shell:waiting', count),
    configure: (c) => ipcRenderer.send('shell:configure', c),
    onNotificationClick(cb) {
      const handler = (_e: unknown, c: NoticeClick) => cb(c)
      ipcRenderer.on('shell:click', handler)
      return () => ipcRenderer.removeListener('shell:click', handler)
    },
  },
  request(hostId, req, onEvent) {
    const id = String(++next)
    listeners.set(id, onEvent)
    ipcRenderer.send('bridge:request', id, hostId, req)
    return () => {
      listeners.delete(id)
      ipcRenderer.send('bridge:abort', id)
    }
  },
}
contextBridge.exposeInMainWorld('desktop', api)
