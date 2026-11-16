import axios from 'axios';
export const API_URL = process.env.NEXT_PUBLIC_API_URL || 'http://localhost:8080';
export const api = axios.create({ baseURL: API_URL });
api.interceptors.request.use((config) => {
  if (typeof window !== 'undefined') {
    const token = localStorage.getItem('token');
    if (token) config.headers.Authorization = `Bearer ${token}`;
  }
  return config;
});
export function getError(error: unknown): string {
  if (axios.isAxiosError(error)) {
    if (typeof error.response?.data?.error === 'string') return error.response.data.error;
    if (typeof error.response?.data === 'string') return error.response.data;
    if (error.message) return error.message;
  }
  if (error instanceof Error) return error.message;
  return 'Something went wrong';
}
