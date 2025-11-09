from fastapi import FastAPI
from pydantic import BaseModel
from sentence_transformers import SentenceTransformer
import joblib
import pandas as pd


app = FastAPI(title="Urgency Classification API")

# Load model + embedder at startup
embedder = SentenceTransformer('all-MiniLM-L6-v2')
model, scaler = joblib.load('/Users/mohammedayaan/Documents/HLLA/prediction_api/models/logreg_with_scaler.pkl')

class MessageInput(BaseModel):
    message: str

@app.post("/predict")
def predict(input_data: MessageInput):
    text = input_data.message
    embedding = embedder.encode([text])
    embedding_scaled = scaler.transform(embedding)
    embedding_df = pd.DataFrame(embedding_scaled, columns=[f"embed_{i}" for i in range(embedding_scaled.shape[1])])
    prediction = model.predict(embedding_df)[0]
    confidence = model.predict_proba(embedding_df).max()
    return {
        "message": text,
        "urgent": bool(prediction),
        "confidence": float(confidence)
    }
