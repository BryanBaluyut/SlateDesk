import { ApiError } from "./client";
import type { Attachment, Problem } from "./types";

/**
 * Server-enforced per-file cap (25 MiB), checked client-side for fast UX.
 * The server budgets the multipart envelope separately, so a file of
 * exactly this size is accepted — the client check and the server limit
 * agree at the boundary.
 */
export const MAX_ATTACHMENT_BYTES = 25 * 1024 * 1024;

export function attachmentDownloadUrl(id: string): string {
  return `/api/v1/attachments/${id}`;
}

/**
 * POST /articles/{id}/attachments as multipart. XMLHttpRequest instead of
 * fetch purely for upload progress events; cookies ride along same-origin.
 */
export function uploadAttachment(
  articleId: string,
  file: File,
  onProgress?: (fraction: number) => void,
): Promise<Attachment> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", `/api/v1/articles/${articleId}/attachments`);
    xhr.responseType = "json";
    xhr.setRequestHeader("Accept", "application/json");

    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable && onProgress) {
        onProgress(e.total > 0 ? e.loaded / e.total : 0);
      }
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(xhr.response as Attachment);
      } else {
        const problem =
          xhr.response && typeof xhr.response === "object"
            ? (xhr.response as Problem)
            : null;
        reject(new ApiError(xhr.status, problem));
      }
    };
    xhr.onerror = () => reject(new Error(`upload of ${file.name} failed`));
    xhr.onabort = () => reject(new Error(`upload of ${file.name} aborted`));

    const form = new FormData();
    form.append("file", file, file.name);
    xhr.send(form);
  });
}
