import { GROUP_KEYS, type GroupDef } from './types';

export const GROUPS: readonly GroupDef[] = GROUP_KEYS.map((key) => ({ key, titleKey: `menu.group.${key}` }));
