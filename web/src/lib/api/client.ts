import { basePath } from "../base-path";
import axios from "axios";
import { getBenchDB } from "./benchdb";

export function createBenchDBClient(baseUrl: string) {
  return getBenchDB(axios.create({ baseURL: baseUrl || basePath(), validateStatus: () => true }));
}
