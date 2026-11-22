import type { InjectionKey } from 'vue';
import type { Registry } from '../registry/registry';

export const REGISTRY_KEY: InjectionKey<Registry> = Symbol('le-admin-registry');
