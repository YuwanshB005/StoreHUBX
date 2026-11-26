"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { useAuth } from "@/lib/store";

// The backend hands the token over in the URL fragment (#token=…&user=…),
// not the query string: a fragment never leaves the browser, so the token
// can't end up in server logs or Referer headers. It is wiped from the
// address bar as soon as it has been read.
export default function AuthCallback() {
  const router = useRouter();
  const setAuth = useAuth((s) => s.setAuth);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const params = new URLSearchParams(window.location.hash.slice(1));
    window.history.replaceState(null, "", window.location.pathname);

    const token = params.get("token");
    const encodedUser = params.get("user"); // base64 of JSON

    if (token && encodedUser) {
      try {
        const user = JSON.parse(atob(encodedUser));
        setAuth(token, user);
        router.replace("/");
      } catch (err) {
        console.error("Invalid user payload:", err);
        setError("Sign-in failed: the callback data was malformed.");
        setTimeout(() => router.replace("/"), 2000);
      }
    } else {
      console.warn("Missing token or user data in callback URL");
      setError("Sign-in failed: missing authentication data.");
      setTimeout(() => router.replace("/"), 2000);
    }
  }, [router, setAuth]);

  return (
    <div className="py-20 text-center opacity-80">
      {error ?? "Completing sign-in…"}
    </div>
  );
}
